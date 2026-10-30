// Package geoip 用离线库（MaxMind GeoLite2）查 IP 的归属地与运营商。
//
// # 为什么是离线的
//
// 在线 IP 情报服务要把「我有哪些机器、分别在哪」这件事讲给第三方听，
// 而这恰好是这类面板最不该外泄的信息。离线库把这件事留在本机。
//
// # 为什么缺库不是错误
//
// GeoLite2 需要使用者自己到 MaxMind 注册并下载，许可不允许随程序分发。
// 所以「没有库」是绝大多数部署的默认状态，而不是异常：Open 在目录或文件
// 不存在时返回 (nil, nil)，调用方拿到 nil 就当作「没启用」，其余功能照常。
// 只有「文件在、但读不出来」才算错误——那是需要用户处理的真问题。
package geoip

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oschwald/maxminddb-golang"
)

// 库文件名。GeoLite2 的压缩包里就是这两个名字，直接解压到目录即可。
const (
	CityFileName = "GeoLite2-City.mmdb"
	ASNFileName  = "GeoLite2-ASN.mmdb"
)

// SourceMMDB 是写入 ip_cache.source 的取值：这条来自离线库。
const SourceMMDB = "mmdb"

// Info 是一个 IP 的归属地。
//
// Found 单独一列而不是靠「字段全空」推断：查不到和查到了但库里的字段确实
// 是空的，在界面上该说不同的话（「这个 IP 不在库里」vs「库里没有城市级信息」）。
type Info struct {
	IP          string  `json:"ip"`
	Found       bool    `json:"found"`
	Country     string  `json:"country"`
	CountryCode string  `json:"countryCode"`
	Region      string  `json:"region"`
	City        string  `json:"city"`
	Latitude    float64 `json:"latitude"`
	Longitude   float64 `json:"longitude"`
	ASN         string  `json:"asn"`
	Org         string  `json:"org"`
	// Source 是这条数据是谁填的：mmdb / manual，空表示查过但没查到。
	Source string `json:"source"`
}

// HasCoords 报告这条记录能不能画到图上。
//
// 0,0 是几内亚湾外的海面——MaxMind 对「只知道国家」的 IP 会返回这个值，
// 或者干脆返回 0。把它当成真实坐标会在图上堆出一个假的热点。
func (i Info) HasCoords() bool {
	return i.Found && !(i.Latitude == 0 && i.Longitude == 0) &&
		i.Latitude >= -90 && i.Latitude <= 90 &&
		i.Longitude >= -180 && i.Longitude <= 180
}

// DB 是加载好的离线库。零值不可用，用 Open 构造。
type DB struct {
	city    *maxminddb.Reader
	asn     *maxminddb.Reader
	dir     string
	builtAt time.Time
}

// Status 描述当前库的状态，供接口与界面显示。
type Status struct {
	Enabled bool   `json:"enabled"`
	Dir     string `json:"dir"`
	// CityBuiltAt 是 City 库的构建日期。GeoLite2 每周更新，界面上显示出来
	// 用户才知道自己那份是不是一年前的。
	CityBuiltAt time.Time `json:"cityBuiltAt"`
	HasCity     bool      `json:"hasCity"`
	HasASN      bool      `json:"hasAsn"`
	// Notice 是给人看的一句话，说明为什么不可用或该做什么。
	Notice string `json:"notice"`
}

// Open 打开目录下的 GeoLite2 库。
//
// 目录或文件不存在时返回 (nil, nil)：那是「没配」，不是错。
// 文件存在但读不出来时返回错误——库下坏了必须让用户知道，否则表现是
// 「所有 IP 都查不到归属地」，而那看起来像本工具的 bug。
func Open(dir string) (*DB, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, nil
	}
	cityPath := filepath.Join(dir, CityFileName)
	if _, err := os.Stat(cityPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("geoip: 检查 %s 失败: %w", CityFileName, err)
	}

	city, err := maxminddb.Open(cityPath)
	if err != nil {
		return nil, fmt.Errorf("geoip: 打开 %s 失败（文件可能损坏或不完整）: %w", CityFileName, err)
	}

	// BuildEpoch 是库的构建时刻（Unix 秒）。界面上要显示它——GeoLite2 每周
	// 更新，用户看到「一年前」才知道该换库了。
	built := time.Unix(int64(city.Metadata.BuildEpoch), 0).UTC()
	db := &DB{city: city, dir: dir, builtAt: built}

	// ASN 库是可选的：没有它也能出国家与城市，只是少了运营商。
	// 所以它读失败只跳过，不让整个功能失效。
	if asn, err := maxminddb.Open(filepath.Join(dir, ASNFileName)); err == nil {
		db.asn = asn
	}
	return db, nil
}

// Close 释放文件句柄。
func (d *DB) Close() error {
	if d == nil {
		return nil
	}
	var errs []error
	if d.city != nil {
		if err := d.city.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if d.asn != nil {
		if err := d.asn.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Status 返回库的状态。nil 的 DB 是合法的，表示未启用。
func (d *DB) Status() Status {
	if d == nil {
		return Status{
			Enabled: false,
			Notice: "未启用 IP 情报。GeoLite2 需要自行到 MaxMind 注册后下载，" +
				"解压出 GeoLite2-City.mmdb 放进数据目录的 geoip/ 下（或设置 " +
				"OCICORE_GEOIP_DIR 指向它），重启后生效。",
		}
	}
	s := Status{
		Enabled:     true,
		Dir:         d.dir,
		CityBuiltAt: d.builtAt,
		HasCity:     true,
		HasASN:      d.asn != nil,
		Notice:      "归属地来自本地离线库，查询不出网。",
	}
	if !s.HasASN {
		s.Notice += "没有找到 GeoLite2-ASN.mmdb，运营商信息会留空。"
	}
	return s
}

// City 是 GeoLite2-City 里我们关心的字段。只取用得到的，解码更快。
type City struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
		Names   struct {
			ZhCN string `maxminddb:"zh-CN"`
			En   string `maxminddb:"en"`
		} `maxminddb:"names"`
	} `maxminddb:"country"`
	Subdivisions []struct {
		Names struct {
			ZhCN string `maxminddb:"zh-CN"`
			En   string `maxminddb:"en"`
		} `maxminddb:"names"`
	} `maxminddb:"subdivisions"`
	City struct {
		Names struct {
			ZhCN string `maxminddb:"zh-CN"`
			En   string `maxminddb:"en"`
		} `maxminddb:"names"`
	} `maxminddb:"city"`
	Location struct {
		Latitude  float64 `maxminddb:"latitude"`
		Longitude float64 `maxminddb:"longitude"`
	} `maxminddb:"location"`
}

// ASNRecord 是 GeoLite2-ASN 的记录。
type ASNRecord struct {
	Number uint   `maxminddb:"autonomous_system_number"`
	Org    string `maxminddb:"autonomous_system_organization"`
}

// Lookup 查一个 IP。
//
// nil 的 DB 返回 Found=false 的空结果而不是报错：调用方在「没配库」和
// 「配了但没查到」两种情况下走的是同一段渲染逻辑，只是文案不同。
func (d *DB) Lookup(ip string) Info {
	out := Info{IP: strings.TrimSpace(ip)}
	if d == nil {
		return out
	}
	addr, err := netip.ParseAddr(out.IP)
	if err != nil {
		return out
	}
	// 私网地址永远查不到，直接给一句明确的结果，别去库里白查一趟。
	if addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
		return out
	}

	var c City
	if err := d.city.Lookup(addr.AsSlice(), &c); err != nil {
		return out
	}
	out.Found = true
	out.Source = SourceMMDB
	out.CountryCode = c.Country.ISOCode
	out.Country = firstNonEmpty(c.Country.Names.ZhCN, c.Country.Names.En, c.Country.ISOCode)
	out.City = firstNonEmpty(c.City.Names.ZhCN, c.City.Names.En)
	if len(c.Subdivisions) > 0 {
		sub := c.Subdivisions[0]
		out.Region = firstNonEmpty(sub.Names.ZhCN, sub.Names.En)
	}
	out.Latitude = c.Location.Latitude
	out.Longitude = c.Location.Longitude

	if d.asn != nil {
		var a ASNRecord
		if err := d.asn.Lookup(addr.AsSlice(), &a); err == nil && a.Number != 0 {
			out.ASN = fmt.Sprintf("AS%d", a.Number)
			out.Org = a.Org
		}
	}
	return out
}

// firstNonEmpty 返回第一个非空串。库里中文名常常缺失，退回英文比留空好。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
