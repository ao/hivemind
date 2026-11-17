package nodeapi

import (
	"bytes"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func TestRouterReportValidate(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	good := RouterReport{
		Requests: []RouterRequests{{Host: "www.example.com", Code: 200, Count: 1}, {Host: UnknownHost, Code: 404, Count: 3}},
		Upstream: []RouterUpstream{{Host: "www.example.com", Kind: "dial"}, {Host: "www.example.com", Kind: "connection"}, {Host: UnknownHost, Kind: "proxy"}},
		Certs:    []RouterCert{{Host: "www.example.com", NotAfter: exp}},
		Refused:  []RouterRefused{{Reason: "address", Count: 4}, {Reason: "total", Count: 1}},
	}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (RouterReport{}).Validate(); err != nil {
		t.Fatalf("an empty report: %v", err)
	}
	for name, bad := range map[string]RouterReport{
		"code 99":         {Requests: []RouterRequests{{Host: "a.example.com", Code: 99}}},
		"code 600":        {Requests: []RouterRequests{{Host: "a.example.com", Code: 600}}},
		"request host":    {Requests: []RouterRequests{{Host: "a b", Code: 200}}},
		"upper case host": {Requests: []RouterRequests{{Host: "A.example.com", Code: 200}}},
		"empty host":      {Requests: []RouterRequests{{Code: 200}}},
		"kind":            {Upstream: []RouterUpstream{{Host: "a.example.com", Kind: "timeout"}}},
		"upstream host":   {Upstream: []RouterUpstream{{Host: "*.example.com", Kind: "dial"}}},
		"cert unknown":    {Certs: []RouterCert{{Host: UnknownHost, NotAfter: exp}}},
		"cert no expiry":  {Certs: []RouterCert{{Host: "a.example.com"}}},
		"too many series": {Requests: make([]RouterRequests, maxRouterSeries+1)},
		"twice requests":  {Requests: []RouterRequests{{Host: "a.example.com", Code: 200, Count: 1}, {Host: "a.example.com", Code: 200, Count: 2}}},
		"twice upstream":  {Upstream: []RouterUpstream{{Host: "a.example.com", Kind: "dial"}, {Host: "a.example.com", Kind: "dial"}}},
		"twice cert":      {Certs: []RouterCert{{Host: "a.example.com", NotAfter: exp}, {Host: "a.example.com", NotAfter: exp}}},
		"refused reason":  {Refused: []RouterRefused{{Reason: "slow"}}},
		"twice refused":   {Refused: []RouterRefused{{Reason: "total", Count: 1}, {Reason: "total", Count: 2}}},
		"too many with refused": {Requests: make([]RouterRequests, maxRouterSeries),
			Refused: []RouterRefused{{Reason: "total"}}},
	} {
		if err := bad.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
}

func TestRouterReportRouted(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	r := RouterReport{
		Requests: []RouterRequests{{Host: "a.example.com", Code: 200, Count: 1}, {Host: "b.example.com", Code: 200, Count: 2}, {Host: UnknownHost, Code: 404, Count: 3}},
		Upstream: []RouterUpstream{{Host: "a.example.com", Kind: "dial", Count: 1}, {Host: "b.example.com", Kind: "dial", Count: 1}, {Host: UnknownHost, Kind: "dial", Count: 1}},
		Certs:    []RouterCert{{Host: "a.example.com", NotAfter: exp}, {Host: "b.example.com", NotAfter: exp}},
		Refused:  []RouterRefused{{Reason: "address", Count: 5}},
	}
	got := r.Routed(map[string]bool{"a.example.com": true})
	// Refused connections have no host, and stay.
	if len(got.Refused) != 1 || got.Refused[0] != (RouterRefused{Reason: "address", Count: 5}) {
		t.Errorf("refused: %+v", got.Refused)
	}
	if len(got.Requests) != 2 || got.Requests[0].Host != "a.example.com" || got.Requests[1].Host != UnknownHost {
		t.Errorf("requests: %+v", got.Requests)
	}
	// The router never counts upstream errors for the unknown host; a report
	// that does is not passed on.
	if len(got.Upstream) != 1 || got.Upstream[0].Host != "a.example.com" {
		t.Errorf("upstream: %+v", got.Upstream)
	}
	if len(got.Certs) != 1 || got.Certs[0].Host != "a.example.com" {
		t.Errorf("certs: %+v", got.Certs)
	}
	if got := r.Routed(nil); len(got.Requests) != 1 || len(got.Upstream) != 0 || len(got.Certs) != 0 {
		t.Errorf("nothing routed: %+v", got)
	}
}

func TestRouterReportsKeepFreshOnes(t *testing.T) {
	rs := NewRouterReports()
	now := time.Now()
	rs.Put("node-a", RouterReport{Requests: []RouterRequests{{Host: UnknownHost, Code: 404, Count: 1}}}, now.Add(-time.Minute))
	rs.Put("node-b", RouterReport{}, now.Add(-3*time.Minute))
	got := rs.Fresh(now, 2*time.Minute)
	if _, ok := got["node-a"]; !ok || len(got) != 1 {
		t.Fatalf("fresh: %+v", got)
	}
	// The stale one is forgotten, not only hidden.
	if got := rs.Fresh(now.Add(-2*time.Minute), time.Hour); len(got) != 1 {
		t.Fatalf("a stale report came back: %+v", got)
	}
	// A newer report replaces the older one.
	rs.Put("node-a", RouterReport{}, now)
	if got := rs.Fresh(now, time.Minute)["node-a"]; len(got.Requests) != 0 {
		t.Fatalf("replaced report: %+v", got)
	}
	var nilRS *RouterReports
	nilRS.Put("node-a", RouterReport{}, now)
	if got := nilRS.Fresh(now, time.Minute); got != nil {
		t.Fatalf("a nil holder kept %+v", got)
	}
}

func TestCountOf(t *testing.T) {
	for _, c := range []struct {
		v    float64
		want uint64
	}{{0, 0}, {-1, 0}, {math.NaN(), 0}, {3, 3}, {3.9, 3}, {math.Inf(1), math.MaxUint64}, {1e30, math.MaxUint64}} {
		if got := CountOf(c.v); got != c.want {
			t.Errorf("CountOf(%v) = %d, want %d", c.v, got, c.want)
		}
	}
}

// FuzzDecodeRouterReport covers the body a worker posts with its router's
// counters. A report that validates has only valid hosts, codes and kinds,
// and what Routed keeps of it still validates.
func FuzzDecodeRouterReport(f *testing.F) {
	f.Add([]byte(`{"requests":[{"host":"www.example.com","code":200,"count":3}],"upstream":[{"host":"www.example.com","kind":"dial","count":1}],"certs":[{"host":"www.example.com","not_after":"2027-01-02T03:04:05Z"}]}`))
	f.Add([]byte(`{"requests":[{"host":"unknown","code":404,"count":1}]}`))
	f.Add([]byte(`{"requests":[{"host":"x\"}","code":200}]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"refused":[{"reason":"address","count":7},{"reason":"total","count":1}]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var r RouterReport
		if err := Decode(bytes.NewReader(data), MaxRouterReportBytes, &r); err != nil {
			return
		}
		if r.Validate() != nil {
			return
		}
		for _, x := range r.Requests {
			if x.Code < 100 || x.Code > 599 || strings.ContainsAny(x.Host, "\"\\\n{}= ") {
				t.Fatalf("accepted %+v", x)
			}
		}
		for _, x := range r.Upstream {
			if strings.ContainsAny(x.Host, "\"\\\n{}= ") || strings.ContainsAny(x.Kind, "\"\\\n") {
				t.Fatalf("accepted %+v", x)
			}
		}
		for _, x := range r.Refused {
			if x.Reason != "address" && x.Reason != "total" {
				t.Fatalf("accepted %+v", x)
			}
		}
		routed := map[string]bool{}
		for _, x := range r.Certs {
			routed[x.Host] = true
		}
		if err := r.Routed(routed).Validate(); err != nil {
			t.Fatalf("a routed part of a valid report is invalid: %v", err)
		}
	})
}
