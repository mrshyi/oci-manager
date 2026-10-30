package httpapi

import (
	"net/http"
	"strings"

	"ocicore/internal/geoip"
	"ocicore/internal/store"
)

// GeoPoint 是图上的一点：一台有公网 IP 的实例。
type GeoPoint struct {
	InstanceID   string  `json:"instanceId"`
	InstanceName string  `json:"instanceName"`
	AccountID    string  `json:"accountId"`
	AccountCode  string  `json:"accountCode"`
	Region       string  `json:"region"`
	IP           string  `json:"ip"`
	Found        bool    `json:"found"`
	Country      string  `json:"country"`
	CountryCode  string  `json:"countryCode"`
	City         string  `json:"city"`
	Latitude     float64 `json:"latitude"`
	Longitude    float64 `json:"longitude"`
	ASN          string  `json:"asn"`
	Org          string  `json:"org"`
	// Source 决定这一点有多准：mmdb（离线库，城市级）、region（按区域估算）、
	// manual（用户手填）、空（查不到）。界面据此用不同的样式，并在图例里写清楚。
	Source string `json:"source"`
}

// handleGetGeoStatus 返回 IP 库的状态。
func (s *Server) handleGetGeoStatus(w http.ResponseWriter, r *http.Request) {
	cached, err := s.st.CountIPInfo(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":       s.geo.Status(),
		"cachedIps":    cached,
		"knownRegions": geoip.KnownRegionCount(),
	})
}

// handleGetGeoDistribution 返回全部实例的地理分布。
//
// 优先用离线库的结果，查不到时退回区域坐标（标成 region）。两条路都走不通
// 的点仍然返回，只是 found=false——图上不画它，但列表里要能看到「这台机器
// 的 IP 我不知道在哪」，而不是让它凭空消失。
func (s *Server) handleGetGeoDistribution(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	insts, err := s.st.ListInstances(ctx, store.InstanceFilter{})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	accounts, err := s.st.ListAccounts(ctx)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	codeByID := make(map[string]string, len(accounts))
	for _, a := range accounts {
		codeByID[a.ID] = a.Code
	}

	// 先读缓存，再补查缺失的。缓存的意义不是省 CPU，而是「换库前后能对比」：
	// 用户换了一份新 GeoLite2，旧结果还在表里，一眼能看出哪些 IP 变了。
	ips := make([]string, 0, len(insts))
	for _, in := range insts {
		if strings.TrimSpace(in.PublicIP) != "" {
			ips = append(ips, in.PublicIP)
		}
	}
	cached, err := s.st.ListIPInfo(ctx, ips)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	points := make([]GeoPoint, 0, len(ips))
	for _, in := range insts {
		ip := strings.TrimSpace(in.PublicIP)
		if ip == "" {
			continue
		}
		p := GeoPoint{
			InstanceID: in.ID, InstanceName: in.DisplayName,
			AccountID: in.AccountID, AccountCode: codeByID[in.AccountID],
			Region: in.Region, IP: ip,
		}

		info, hit := cached[ip]
		if !hit && s.geo != nil {
			// 库里没有就查一次并落库。写回失败不影响本次返回——用户已经
			// 看到结果了，下次再补。
			live := s.geo.Lookup(ip)
			info = store.IPInfo{
				IP: live.IP, Found: live.Found,
				Country: live.Country, CountryCode: live.CountryCode,
				Region: live.Region, City: live.City,
				Latitude: live.Latitude, Longitude: live.Longitude,
				ASN: live.ASN, Org: live.Org, Source: live.Source,
			}
			_ = s.st.UpsertIPInfo(ctx, info)
		}

		if info.Found {
			p.Found = true
			p.Country = info.Country
			p.CountryCode = info.CountryCode
			p.City = info.City
			p.Latitude = info.Latitude
			p.Longitude = info.Longitude
			p.ASN = info.ASN
			p.Org = info.Org
			p.Source = info.Source
		}

		// 离线库没结果（或没配库）时退回区域坐标。它不够精确，但比空白有用，
		// 而且 source 明确写着 region，用户不会把它当成精确位置。
		if !p.Found || (p.Latitude == 0 && p.Longitude == 0) {
			if lat, lon, ok := geoip.RegionCoords(in.Region); ok {
				p.Latitude, p.Longitude = lat, lon
				if p.Source == "" {
					p.Source = "region"
				}
				if !p.Found {
					p.Found = true
				}
			}
		}

		points = append(points, p)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status": s.geo.Status(),
		"points": points,
		"notice": "坐标来自本地离线库；没有离线库时按 OCI 区域估算，" +
			"同一区域的多台机器会落在同一个点上。图里不画国界，只按经纬度定位。",
	})
}

// geoManualRequest 手工填一条归属地。
type geoManualRequest struct {
	IP        string  `json:"ip"`
	Country   string  `json:"country"`
	City      string  `json:"city"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// handleSetGeoManual 手工指定某个 IP 的位置。
//
// 手填的条目不会被自动查询覆盖（store 层用 WHERE source != 'manual' 保证）。
// 这条路径存在的原因很具体：刚分配的地址段常常还没进 GeoLite2，而用户
// 恰好知道那台机器在哪个城市。
func (s *Server) handleSetGeoManual(w http.ResponseWriter, r *http.Request) {
	var req geoManualRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ip := strings.TrimSpace(req.IP)
	if ip == "" {
		writeError(w, http.StatusBadRequest, "invalid_ip", "缺少 IP")
		return
	}
	if req.Latitude < -90 || req.Latitude > 90 || req.Longitude < -180 || req.Longitude > 180 {
		writeError(w, http.StatusBadRequest, "invalid_coords", "经纬度超出范围")
		return
	}

	info := store.IPInfo{
		IP: ip, Found: true, Country: req.Country, City: req.City,
		Latitude: req.Latitude, Longitude: req.Longitude,
	}
	if err := s.st.SetIPInfoManual(r.Context(), info); err != nil {
		writeStoreError(w, err)
		return
	}

	user := userFrom(r.Context())
	_ = s.st.Audit(r.Context(), store.AuditEntry{
		UserID: user.ID, Action: "geo_manual_set", Target: ip,
		IP: s.clientIP(r), Detail: req.Country + " " + req.City,
	})

	got, err := s.st.GetIPInfo(r.Context(), ip)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"info": got})
}

// handleDeleteGeoManual 撤销手工指定的位置。
func (s *Server) handleDeleteGeoManual(w http.ResponseWriter, r *http.Request) {
	ip := r.PathValue("ip")
	if err := s.st.DeleteIPInfo(r.Context(), ip); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
