package geoip

import "strings"

// 区域坐标表。
//
// # 为什么需要它
//
// GeoLite2 要用户自己去 MaxMind 注册下载，不能随程序分发——也就是说
// 绝大多数部署里是「没有库」的状态。如果图上一片空白，这个功能等于没做。
// OCI 的区域名本身就带地理信息，用它给一个大致位置，图立刻就有内容了。
//
// # 为什么这是「估算」而不是「查询」
//
// 区域坐标是数据中心所在城市的中心点，同一区域里的几台机器会落在同一个
// 点上。所以结果里 source 标成 region 而不是 mmdb，界面上也据此说明
// 「按区域估算，精确到城市请配置离线库」。把两者混为一谈会让用户以为
// 面板知道机器在哪栋楼里。
//
// 坐标只用于在示意图上定位，不用于任何计算，因此取城市中心即可。
var regionCoords = map[string][2]float64{
	// 北美
	"us-ashburn-1":   {39.04, -77.49},
	"us-phoenix-1":   {33.45, -112.07},
	"us-sanjose-1":   {37.34, -121.89},
	"us-chicago-1":   {41.88, -87.63},
	"ca-toronto-1":   {43.65, -79.38},
	"ca-montreal-1":  {45.50, -73.57},
	"mx-queretaro-1": {20.59, -100.39},
	"mx-monterrey-1": {25.69, -100.32},

	// 南美
	"sa-saopaulo-1":    {-23.55, -46.63},
	"sa-vinhedo-1":     {-23.03, -47.02},
	"sa-santiago-1":    {-33.45, -70.67},
	"sa-valparaiso-1":  {-33.05, -71.62},
	"sa-bogota-1":      {4.71, -74.07},
	"sa-lima-1":        {-12.05, -77.04},
	"sa-buenosaires-1": {-34.60, -58.38},

	// 欧洲
	"uk-london-1":     {51.51, -0.13},
	"uk-cardiff-1":    {51.48, -3.18},
	"eu-frankfurt-1":  {50.11, 8.68},
	"eu-amsterdam-1":  {52.37, 4.90},
	"eu-zurich-1":     {47.37, 8.54},
	"eu-madrid-1":     {40.42, -3.70},
	"eu-marseille-1":  {43.30, 5.37},
	"eu-milan-1":      {45.46, 9.19},
	"eu-stockholm-1":  {59.33, 18.07},
	"eu-paris-1":      {48.86, 2.35},
	"eu-madrid-3":     {40.42, -3.70},
	"eu-frankfurt-2":  {50.11, 8.68},
	"eu-jovanovac-1":  {45.34, 19.99},

	// 亚太
	"ap-tokyo-1":     {35.68, 139.69},
	"ap-osaka-1":     {34.69, 135.50},
	"ap-seoul-1":     {37.57, 126.98},
	"ap-chuncheon-1": {37.88, 127.73},
	"ap-mumbai-1":    {19.08, 72.88},
	"ap-hyderabad-1": {17.39, 78.49},
	"ap-singapore-1": {1.35, 103.82},
	"ap-singapore-2": {1.35, 103.82},
	"ap-sydney-1":    {-33.87, 151.21},
	"ap-melbourne-1": {-37.81, 144.96},

	// 中东与非洲
	"me-jeddah-1":        {21.49, 39.19},
	"me-riyadh-1":        {24.71, 46.68},
	"me-dubai-1":         {25.20, 55.27},
	"me-abudhabi-1":      {24.45, 54.38},
	"af-johannesburg-1":  {-26.20, 28.05},
	"af-casablanca-1":    {33.57, -7.59},
	"il-jerusalem-1":     {31.77, 35.21},
}

// RegionCoords 返回 OCI 区域名对应的大致坐标。
//
// 未收录的区域返回 false，调用方据此退回「只显示国家/区域名」而不是
// 硬塞一个 0,0——那会在图上堆出一个几内亚湾的假热点。
func RegionCoords(region string) (lat, lon float64, ok bool) {
	c, ok := regionCoords[strings.ToLower(strings.TrimSpace(region))]
	if !ok {
		return 0, 0, false
	}
	return c[0], c[1], true
}

// KnownRegionCount 返回表里收录的区域数，供测试与状态接口显示。
func KnownRegionCount() int { return len(regionCoords) }
