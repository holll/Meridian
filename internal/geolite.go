package internal

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lionsoul2014/ip2region/binding/golang/xdb"
	"github.com/oschwald/maxminddb-golang"
)

const (
	geoliteCountryFile = "GeoLite2-Country.mmdb"
	geoliteASNFile     = "GeoLite2-ASN.mmdb"
	geoliteXDBFile     = "ip2region.xdb"
	geoliteCountryURL  = "https://github.com/P3TERX/GeoLite.mmdb/raw/download/GeoLite2-Country.mmdb"
	geoliteASNURL      = "https://github.com/P3TERX/GeoLite.mmdb/raw/download/GeoLite2-ASN.mmdb"
	geoliteXDBURL      = "https://raw.githubusercontent.com/lionsoul2014/ip2region/master/data/ip2region_v4.xdb"
)

// GeoInfo is the geolocation and ASN attribution for one IP address.
type GeoInfo struct {
	Country     string `json:"country,omitempty"` // Chinese name preferred
	CountryCode string `json:"country_code,omitempty"`
	Province    string `json:"province,omitempty"`
	City        string `json:"city,omitempty"`
	ASN         uint32 `json:"asn,omitempty"`
	Org         string `json:"org,omitempty"` // ASN organization (ISP)
}

// GeoLite holds the loaded IP attribution databases. Nil values are safe to query.
type GeoLite struct {
	country *maxminddb.Reader
	asn     *maxminddb.Reader
	ip2     *xdb.Searcher // ip2region for CN province/city (MaxMind free lacks it)
	ip2mu   sync.Mutex    // xdb.Searcher is not thread-safe
}

// OpenGeoLite loads the GeoLite2 Country/ASN and ip2region databases from dir
// (the binary's directory when dir is empty), downloading them when missing.
// Returns nil with a logged warning when unavailable so the panel keeps
// working without geolocation.
func OpenGeoLite(dir string) *GeoLite {
	if dir == "" {
		if exe, err := os.Executable(); err == nil {
			dir = filepath.Dir(exe)
		} else {
			dir = "."
		}
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("[geolite] cannot use dir %s: %v", dir, err)
		return nil
	}
	country := loadOrDownload(filepath.Join(dir, geoliteCountryFile), geoliteCountryURL)
	asn := loadOrDownload(filepath.Join(dir, geoliteASNFile), geoliteASNURL)
	ip2 := loadXDB(filepath.Join(dir, geoliteXDBFile), geoliteXDBURL)
	if country == nil && asn == nil && ip2 == nil {
		return nil
	}
	log.Printf("[geolite] loaded Country+ASN+ip2region databases from %s", dir)
	return &GeoLite{country: country, asn: asn, ip2: ip2}
}

func (g *GeoLite) Close() {
	if g == nil {
		return
	}
	if g.country != nil {
		g.country.Close()
	}
	if g.asn != nil {
		g.asn.Close()
	}
}

// Lookup returns attribution for an IP string, or nil for private/unresolvable
// addresses and when no databases are available.
func (g *GeoLite) Lookup(ipStr string) *GeoInfo {
	if g == nil {
		return nil
	}
	ip := net.ParseIP(strings.TrimSpace(ipStr))
	if ip == nil || !ip.IsGlobalUnicast() {
		return nil
	}
	info := &GeoInfo{}
	if g.country != nil {
		var rec struct {
			Country struct {
				ISOCode string            `maxminddb:"iso_code"`
				Names   map[string]string `maxminddb:"names"`
			} `maxminddb:"country"`
		}
		if err := g.country.Lookup(ip, &rec); err == nil {
			info.CountryCode = rec.Country.ISOCode
			info.Country = preferredGeoName(rec.Country.Names)
		}
	}
	// ip2region provides province/city for Chinese IPs that MaxMind's free
	// databases lack. Region format: 国家|省份|城市|ISP.
	if g.ip2 != nil {
		g.ip2mu.Lock()
		region, err := g.ip2.Search(ipStr)
		g.ip2mu.Unlock()
		if err == nil {
			parts := strings.Split(region, "|")
			if len(parts) >= 2 && parts[1] != "0" && parts[1] != "" {
				info.Province = parts[1]
			}
			if len(parts) >= 3 && parts[2] != "0" && parts[2] != "" {
				info.City = parts[2]
			}
		}
	}
	if g.asn != nil {
		var rec struct {
			Number uint32 `maxminddb:"autonomous_system_number"`
			Org    string `maxminddb:"autonomous_system_organization"`
		}
		if err := g.asn.Lookup(ip, &rec); err == nil {
			info.ASN = rec.Number
			info.Org = rec.Org
		}
	}
	if info.CountryCode == "" && info.City == "" && info.ASN == 0 {
		return nil
	}
	return info
}

// DetectISP maps an IP's ASN attribution to the panel's operator labels
// (telecom/unicom/mobile/hk/oversea). Returns "" when unknown or unavailable.
func DetectISP(geo *GeoInfo) string {
	if geo == nil {
		return ""
	}
	org := strings.ToLower(geo.Org)
	switch {
	case strings.Contains(org, "china telecom"), strings.Contains(org, "chinanet"):
		return "telecom"
	case strings.Contains(org, "china unicom"):
		return "unicom"
	case strings.Contains(org, "china mobile"):
		return "mobile"
	}
	if geo.CountryCode == "HK" {
		return "hk"
	}
	return "oversea"
}

// GeoAgg is one country or ISP (ASN org) aggregation bucket.
type GeoAgg struct {
	Name  string `json:"name"`
	Code  string `json:"code,omitempty"` // country ISO code, ISP buckets omit it
	Count int64  `json:"count"`
	Bytes int64  `json:"bytes"`
}

// AggregateGeo folds per-IP aggregations into region (city preferred, country
// fallback) and ISP totals, both sorted by request count (top 20). China's
// three big ISPs are normalized to Chinese labels so their many ASN org names
// (e.g. "Chinanet", "China Telecom (Group)") roll up into one bucket.
func AggregateGeo(aggs []AccessLogIPAgg, gl *GeoLite) (regions, orgs []GeoAgg) {
	regionMap := make(map[string]*GeoAgg)
	orgMap := make(map[string]*GeoAgg)
	for _, a := range aggs {
		geo := gl.Lookup(a.IP)
		if geo == nil {
			continue
		}
		name := geo.City
		if name == "" {
			name = geo.Country
		}
		if name == "" {
			name = geo.CountryCode
		}
		if name != "" {
			r := regionMap[name]
			if r == nil {
				regionMap[name] = &GeoAgg{Name: name, Code: geo.CountryCode, Count: a.Count, Bytes: a.BytesOut}
			} else {
				r.Count += a.Count
				r.Bytes += a.BytesOut
			}
		}
		if geo.Org != "" {
			orgKey := geo.Org
			switch DetectISP(geo) {
			case "telecom":
				orgKey = "电信"
			case "unicom":
				orgKey = "联通"
			case "mobile":
				orgKey = "移动"
			}
			o := orgMap[orgKey]
			if o == nil {
				orgMap[orgKey] = &GeoAgg{Name: orgKey, Count: a.Count, Bytes: a.BytesOut}
			} else {
				o.Count += a.Count
				o.Bytes += a.BytesOut
			}
		}
	}
	regions = topGeoAggs(regionMap)
	orgs = topGeoAggs(orgMap)
	return
}

func topGeoAggs(m map[string]*GeoAgg) []GeoAgg {
	out := make([]GeoAgg, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	if len(out) > 20 {
		out = out[:20]
	}
	if out == nil {
		out = []GeoAgg{}
	}
	return out
}

// preferredGeoName picks the Chinese name when available, falling back to English.
func preferredGeoName(names map[string]string) string {
	if names == nil {
		return ""
	}
	if v := names["zh-CN"]; v != "" {
		return v
	}
	return names["en"]
}

// ensureDownloaded downloads url into path when the file is missing.
func ensureDownloaded(path, url string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	log.Printf("[geolite] %s not found, downloading from %s", filepath.Base(path), url)
	if err := downloadFile(path, url); err != nil {
		return fmt.Errorf("download %s: %w", filepath.Base(path), err)
	}
	return nil
}

func loadOrDownload(path, url string) *maxminddb.Reader {
	if err := ensureDownloaded(path, url); err != nil {
		log.Printf("[geolite] %v", err)
		return nil
	}
	reader, err := maxminddb.Open(path)
	if err != nil {
		log.Printf("[geolite] open %s failed: %v", path, err)
		return nil
	}
	return reader
}

func loadXDB(path, url string) *xdb.Searcher {
	if err := ensureDownloaded(path, url); err != nil {
		log.Printf("[geolite] %v", err)
		return nil
	}
	buffer, err := xdb.LoadContentFromFile(path)
	if err != nil {
		log.Printf("[geolite] load %s failed: %v", path, err)
		return nil
	}
	searcher, err := xdb.NewWithBuffer(xdb.IPv4, buffer)
	if err != nil {
		log.Printf("[geolite] init %s failed: %v", path, err)
		return nil
	}
	return searcher
}

// downloadFile fetches url into path via a temp file so a failed download
// never leaves a half-written database behind.
func downloadFile(path, url string) error {
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	tmp := path + ".download"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		os.Remove(tmp)
		return closeErr
	}
	return os.Rename(tmp, path)
}
