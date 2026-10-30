package httpapi

import (
	"context"
	"net/http"
	"sync"
	"time"

	"ocicore/internal/accountsvc"
	"ocicore/internal/billingsvc"
	"ocicore/internal/ociclient"
	"ocicore/internal/store"
)

// billingRiskTimeout 是整个取数的上限。
//
// 六路取数里有四路要打 OCI（三项限额 + 公网 IP + NLB + 账单），
// 给 40 秒：单路超时是 25-30 秒，并发跑完不该超过它太多。
const billingRiskTimeout = 40 * time.Second

// handleGetBillingRisk 回答一个问题：「这个账号上有没有东西正在扣钱」。
//
// 六条规则的判定在 accountsvc 里（纯函数），这里只负责把数据取齐。
// 取不到的项不隐藏——取不到不等于没风险，界面要能说「这项没查到」。
func (s *Server) handleGetBillingRisk(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	acc, err := s.st.GetAccount(ctx, r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}

	instances, err := s.st.ListInstances(ctx, store.InstanceFilter{AccountIDs: []string{acc.ID}})
	if err != nil {
		writeStoreError(w, err)
		return
	}

	in := accountsvc.RiskInput{
		Account:      *acc,
		Instances:    instances,
		FreeEgressGB: billingsvc.FreeEgressGB,
		Errors:       map[string]string{},
	}

	client, clientErr := s.conns.For(ctx, acc)
	if clientErr != nil {
		// 连客户端都建不出来（私钥解不开）时，只有本地能算的那几项还算得出来。
		// 其余五项全部标"未查到"，而不是当成"没有风险"——那会给出一个
		// 虚假的"一切正常"。
		msg := "读取凭据失败: " + clientErr.Error()
		for _, key := range []string{
			accountsvc.RiskARMOver, accountsvc.RiskMicroOver, accountsvc.RiskBlockOver,
			accountsvc.RiskNLB, accountsvc.RiskReservedPublicIP, accountsvc.RiskCrossRegionEgress,
		} {
			in.Errors[key] = msg
		}
		writeJSON(w, http.StatusOK, accountsvc.AssessBillingRisk(in))
		return
	}

	ctx, cancel := context.WithTimeout(ctx, billingRiskTimeout)
	defer cancel()

	region := acc.DefaultRegion
	if region == "" {
		region = client.Region()
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	setErr := func(key, msg string) {
		mu.Lock()
		defer mu.Unlock()
		in.Errors[key] = msg
	}
	setSnap := func(dst *accountsvc.LimitSnapshot, snap accountsvc.LimitSnapshot) {
		mu.Lock()
		defer mu.Unlock()
		*dst = snap
	}

	// 三项限额：复用配额页那套逻辑，作用域不能写错——
	// ARM 与块存储是 REGION 作用域，E2 Micro 是 AD 作用域，
	// 传反了会拿到 400 而不是数据。
	wg.Add(3)
	go func() {
		defer wg.Done()
		item := fetchQuotaItem(ctx, client, region, acc.CompartmentOCID,
			ociclient.LimitServiceCompute, ociclient.LimitARMCores, "ARM OCPU", ociclient.ScopeRegion)
		if !item.Known {
			setErr(accountsvc.RiskARMOver, item.Error)
			return
		}
		setSnap(&in.ARM, accountsvc.LimitSnapshot{
			Used: item.Used, Limit: item.Limit, Known: true,
		})
	}()
	go func() {
		defer wg.Done()
		item := fetchQuotaItem(ctx, client, region, acc.CompartmentOCID,
			ociclient.LimitServiceCompute, ociclient.LimitE2MicroVMs, "AMD 微型实例", ociclient.ScopeAD)
		if !item.Known {
			setErr(accountsvc.RiskMicroOver, item.Error)
			return
		}
		setSnap(&in.Micro, accountsvc.LimitSnapshot{
			Used: item.Used, Limit: item.Limit, Known: true,
		})
	}()
	go func() {
		defer wg.Done()
		item := fetchQuotaItem(ctx, client, region, acc.CompartmentOCID,
			ociclient.LimitServiceBlockStorage, ociclient.LimitStorageGB, "块存储", ociclient.ScopeRegion)
		if !item.Known {
			setErr(accountsvc.RiskBlockOver, item.Error)
			return
		}
		setSnap(&in.Block, accountsvc.LimitSnapshot{
			Used: item.Used, Limit: item.Limit, Known: true,
		})
	}()

	// 保留公网 IP。临时 IP 挂在运行中的实例上不额外收费，
	// 只有 RESERVED 才按小时计费，所以这里按 lifetime 过滤。
	wg.Add(1)
	go func() {
		defer wg.Done()
		ips, err := client.ListPublicIPs(ctx, region, acc.CompartmentOCID, "REGION", "")
		if err != nil {
			setErr(accountsvc.RiskReservedPublicIP, shortOCIError(err))
			return
		}
		n := 0
		for _, ip := range ips {
			if ip.Lifetime == "RESERVED" {
				n++
			}
		}
		mu.Lock()
		in.ReservedPublicIPs = n
		mu.Unlock()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		nlbs, err := client.ListNetworkLoadBalancers(ctx, region, acc.CompartmentOCID)
		if err != nil {
			setErr(accountsvc.RiskNLB, shortOCIError(err))
			return
		}
		n := 0
		for _, nlb := range nlbs {
			// 正在删除的 NLB 仍在计费，直到它真的消失。
			if nlb.LifecycleState != ociclient.NLBLifecycleDeleted {
				n++
			}
		}
		mu.Lock()
		in.NLBCount = n
		mu.Unlock()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if s.billing == nil {
			setErr(accountsvc.RiskCrossRegionEgress, "账单服务不可用")
			return
		}
		// 走缓存：Usage API 的数据本身有几小时延迟，刷得再勤也变不出新数字。
		rep := s.billing.EgressFor(ctx, acc, false)
		switch rep.Status {
		case billingsvc.StatusOK, billingsvc.StatusFree:
			mu.Lock()
			in.EgressGB = rep.UsedGB
			in.EgressKnown = true
			mu.Unlock()
		case billingsvc.StatusNoPermission:
			setErr(accountsvc.RiskCrossRegionEgress, "缺少 read usage-report 权限，无法查询出站流量")
		case billingsvc.StatusDisabled:
			setErr(accountsvc.RiskCrossRegionEgress, "账号已停用")
		default:
			msg := rep.Error
			if msg == "" {
				msg = "账单数据查询失败"
			}
			setErr(accountsvc.RiskCrossRegionEgress, msg)
		}
	}()

	wg.Wait()
	writeJSON(w, http.StatusOK, accountsvc.AssessBillingRisk(in))
}
