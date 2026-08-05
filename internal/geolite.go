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
	"time"

	"github.com/oschwald/maxminddb-golang"
)

const (
	geoliteCountryFile = "GeoLite2-Country.mmdb"
	geoliteASNFile     = "GeoLite2-ASN.mmdb"
	geoliteCountryURL  = "https://github.com/P3TERX/GeoLite.mmdb/raw/download/GeoLite2-Country.mmdb"
	geoliteASNURL      = "https://github.com/P3TERX/GeoLite.mmdb/raw/download/GeoLite2-ASN.mmdb"
)

// GeoInfo is the geolocation and ASN attribution for one IP address.
type GeoInfo struct {
	Country     string `json:"country,omitempty"` // Chinese name preferred
	CountryCode string `json:"country_code,omitempty"`
	ASN         uint32 `json:"asn,omitempty"`
	Org         string `json:"org,omitempty"` // ASN organization (ISP)
}

// GeoLite holds the loaded MaxMind databases. Nil values are safe to query.
type GeoLite struct {
	country *maxminddb.Reader
	asn     *maxminddb.Reader
}

// OpenGeoLite loads the GeoLite2 Country and ASN databases from dir (the
// binary's directory when dir is empty), downloading them when missing.
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
	if err := os.MkdirAll(dir, 0750); err != nil {
		log.Printf("[geolite] cannot use dir %s: %v", dir, err)
		return nil
	}
	country := loadOrDownload(filepath.Join(dir, geoliteCountryFile), geoliteCountryURL)
	asn := loadOrDownload(filepath.Join(dir, geoliteASNFile), geoliteASNURL)
	if country == nil && asn == nil {
		return nil
	}
	log.Printf("[geolite] loaded Country+ASN databases from %s", dir)
	return &GeoLite{country: country, asn: asn}
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
	if info.CountryCode == "" && info.ASN == 0 {
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

// AggregateGeo folds per-IP aggregations into country and ISP totals, both
// sorted by request count (top 20). China's three big ISPs are normalized to
// Chinese labels so their many ASN org names (e.g. "Chinanet", "China Telecom
// (Group)") roll up into one bucket.
func AggregateGeo(aggs []AccessLogIPAgg, gl *GeoLite) (regions, orgs []GeoAgg) {
	regionMap := make(map[string]*GeoAgg)
	orgMap := make(map[string]*GeoAgg)
	for _, a := range aggs {
		geo := gl.Lookup(a.IP)
		if geo == nil {
			continue
		}
		name := geo.Country
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
				orgKey = "中国电信"
			case "unicom":
				orgKey = "中国联通"
			case "mobile":
				orgKey = "中国移动"
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
	f, err := os.Create(tmp) // #nosec G304 -- path is a fixed database filename under the operator-configured directory
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
