package geoip

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 没配库时 Open 返回 (nil, nil)：这是「没启用」，不是错误。
// 做成错误的话，绝大多数部署第一次就跑不起来——GeoLite2 需要用户自己去
// MaxMind 注册下载，不能随程序分发。
func TestOpenWithoutDirIsNotAnError(t *testing.T) {
	for _, dir := range []string{"", "   ", filepath.Join(t.TempDir(), "not-here")} {
		db, err := Open(dir)
		if err != nil {
			t.Fatalf("Open(%q) 返回错误: %v", dir, err)
		}
		if db != nil {
			t.Fatalf("Open(%q) 应返回 nil", dir)
		}
	}
}

// 目录里只有 ASN 库没有 City 库时也算没配：City 才是出坐标的那个。
func TestOpenRequiresCityFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ASNFileName), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("缺 City 库不该报错: %v", err)
	}
	if db != nil {
		t.Fatal("缺 City 库应返回 nil")
	}
}

// 库文件在、但读不出来时必须报错。
//
// 那种情况的表现是「所有 IP 都查不到归属地」，看起来像本工具的 bug；
// 静默降级会让人一直查错方向。
func TestOpenCorruptFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, CityFileName), []byte("not-a-mmdb"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := Open(dir)
	if err == nil {
		if db != nil {
			_ = db.Close()
		}
		t.Fatal("损坏的库文件应报错")
	}
	if !strings.Contains(err.Error(), CityFileName) {
		t.Errorf("错误应点名是哪个文件: %v", err)
	}
}

// nil 的 DB 是合法值，Status 与 Lookup 都要能安全调用。
func TestNilDBSafety(t *testing.T) {
	var db *DB
	s := db.Status()
	if s.Enabled {
		t.Error("nil DB 不该报告已启用")
	}
	if !strings.Contains(s.Notice, "GeoLite2") {
		t.Errorf("提示语应告诉用户该放什么文件: %s", s.Notice)
	}
	if !strings.Contains(s.Notice, "geoip") {
		t.Errorf("提示语应告诉用户放在哪: %s", s.Notice)
	}

	info := db.Lookup("1.1.1.1")
	if info.Found {
		t.Error("nil DB 查不出结果")
	}
	if info.IP != "1.1.1.1" {
		t.Errorf("IP 应原样带回: %q", info.IP)
	}
	if err := db.Close(); err != nil {
		t.Errorf("nil DB 的 Close 不该报错: %v", err)
	}
}

// 私网与回环地址永远查不到，不必去库里白查一趟。
func TestLookupSkipsPrivateAddresses(t *testing.T) {
	db := &DB{} // 故意不带 city reader：走不到查询就说明被提前拦住了
	for _, ip := range []string{"10.0.0.1", "192.168.1.1", "127.0.0.1", "172.16.0.1", "::1", "fe80::1"} {
		if info := db.Lookup(ip); info.Found {
			t.Errorf("%s 不该有归属地", ip)
		}
	}
}

// 非法的 IP 字符串不该 panic。
func TestLookupInvalidIP(t *testing.T) {
	db := &DB{}
	for _, ip := range []string{"", "not-an-ip", "999.1.1.1", "  "} {
		if info := db.Lookup(ip); info.Found {
			t.Errorf("%q 不该有归属地", ip)
		}
	}
}

// 0,0 是「只知道国家」的占位，不能当成真实坐标画到图上。
func TestHasCoordsRejectsZeroZero(t *testing.T) {
	cases := []struct {
		name string
		info Info
		want bool
	}{
		{"没查到", Info{Found: false}, false},
		{"零点", Info{Found: true, Latitude: 0, Longitude: 0}, false},
		{"东京", Info{Found: true, Latitude: 35.68, Longitude: 139.69}, true},
		{"南半球", Info{Found: true, Latitude: -33.87, Longitude: 151.21}, true},
		{"纬度越界", Info{Found: true, Latitude: 91, Longitude: 0.1}, false},
		{"经度越界", Info{Found: true, Latitude: 10, Longitude: 181}, false},
	}
	for _, c := range cases {
		if got := c.info.HasCoords(); got != c.want {
			t.Errorf("%s: HasCoords() = %v，期望 %v", c.name, got, c.want)
		}
	}
}

// 区域坐标表覆盖常用区域，且未收录的区域如实返回 false。
func TestRegionCoords(t *testing.T) {
	known := []string{"ap-tokyo-1", "us-ashburn-1", "eu-frankfurt-1", "ap-sydney-1"}
	for _, r := range known {
		lat, lon, ok := RegionCoords(r)
		if !ok {
			t.Errorf("%s 应有坐标", r)
			continue
		}
		if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
			t.Errorf("%s 的坐标越界: %v,%v", r, lat, lon)
		}
	}

	// 大小写与空白要能吃进去：区域名来自配置，用户可能手抄。
	if _, _, ok := RegionCoords("  AP-TOKYO-1 "); !ok {
		t.Error("区域名应做归一化")
	}

	if _, _, ok := RegionCoords("xx-nowhere-1"); ok {
		t.Error("未收录的区域不该返回坐标——那会堆出一个几内亚湾的假热点")
	}
	if _, _, ok := RegionCoords(""); ok {
		t.Error("空区域名不该返回坐标")
	}

	if KnownRegionCount() < 30 {
		t.Errorf("区域表只有 %d 条，覆盖不足", KnownRegionCount())
	}
}

// 表里每一条都得落在合法范围内。
//
// 刻意不检查「坐标互不相同」：同一座城市的多个区域（eu-frankfurt-1/-2、
// ap-singapore-1/-2）本来就该是同一个点，要求它们不同反而会逼人编造坐标。
func TestRegionCoordsInRange(t *testing.T) {
	for r, c := range regionCoords {
		if c[0] < -90 || c[0] > 90 {
			t.Errorf("%s 的纬度越界: %v", r, c[0])
		}
		if c[1] < -180 || c[1] > 180 {
			t.Errorf("%s 的经度越界: %v", r, c[1])
		}
		if c[0] == 0 && c[1] == 0 {
			t.Errorf("%s 落在 0,0——那通常是「不知道」的占位值", r)
		}
	}
}
