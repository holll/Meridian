package internal

import "testing"

func TestDetectISPClassifiesOperators(t *testing.T) {
	cases := []struct {
		org  string
		code string
		want string
	}{
		{"Chinanet", "CN", "telecom"},
		{"China Telecom (Group)", "CN", "telecom"},
		{"China Unicom Beijing Province Network", "CN", "unicom"},
		{"China Mobile Communications Corporation", "CN", "mobile"},
		{"HKT Limited", "HK", "hk"},
		{"AT&T Services, Inc.", "US", "oversea"},
		{"", "US", "oversea"},
	}
	for _, c := range cases {
		if got := DetectISP(&GeoInfo{Org: c.org, CountryCode: c.code}); got != c.want {
			t.Errorf("DetectISP(org=%q code=%q) = %q, want %q", c.org, c.code, got, c.want)
		}
	}
	if got := DetectISP(nil); got != "" {
		t.Fatalf("DetectISP(nil) = %q, want empty", got)
	}
}

func TestAggregateGeoSkipsUnattributedIPs(t *testing.T) {
	aggs := []AccessLogIPAgg{
		{IP: "1.1.1.1", Count: 3, BytesOut: 30},
		{IP: "127.0.0.1", Count: 1, BytesOut: 10}, // private: lookup returns nil
	}
	regions, orgs := AggregateGeo(aggs, &GeoLite{}) // no databases loaded
	if len(regions) != 0 || len(orgs) != 0 {
		t.Fatalf("regions=%d orgs=%d with empty GeoLite, want 0/0", len(regions), len(orgs))
	}
}

func TestTopGeoAggsSortsAndCapsAtTwenty(t *testing.T) {
	m := map[string]*GeoAgg{}
	for i := 0; i < 25; i++ {
		m[string(rune('a'+i))] = &GeoAgg{Name: string(rune('a' + i)), Count: int64(25 - i)}
	}
	out := topGeoAggs(m)
	if len(out) != 20 {
		t.Fatalf("top geo aggs = %d, want 20", len(out))
	}
	if out[0].Count != 25 || out[19].Count != 6 {
		t.Fatalf("sorted counts = %d..%d, want 25..6", out[0].Count, out[19].Count)
	}
}
