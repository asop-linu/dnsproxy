package proxy

import (
	"log/slog"
	"net"
	"testing"

	"github.com/miekg/dns"
)

// benchUpstream is a mock upstream for benchmarks.
var benchUpstream = &testUpstream{
	OnAddress:  func() (addr string) { return "bench-upstream" },
	OnExchange: func(m *dns.Msg) (*dns.Msg, error) { return nil, nil },
	OnClose:    func() error { return nil },
}

// benchLogger is a discard logger for benchmarks.
var benchLogger = slog.Default()

func newBenchMsg() (req, resp *dns.Msg) {
	req = (&dns.Msg{}).SetQuestion("google.com.", dns.TypeA)
	req.SetEdns0(4096, false)

	resp = (&dns.Msg{}).SetReply(req)
	resp.SetEdns0(4096, false)
	resp.Answer = []dns.RR{
		&dns.A{
			Hdr: dns.RR_Header{
				Name:   "google.com.",
				Rrtype: dns.TypeA,
				Class:  dns.ClassINET,
				Ttl:    300,
			},
			A: net.IP{8, 8, 8, 8},
		},
	}

	return req, resp
}

func BenchmarkMsgToKey(b *testing.B) {
	req, _ := newBenchMsg()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = msgToKey(req)
	}
}

func BenchmarkMsgToKeyWithSubnet(b *testing.B) {
	req, _ := newBenchMsg()
	ip := net.IP{192, 168, 1, 0}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = msgToKeyWithSubnet(req, ip, 24)
	}
}

func BenchmarkCacheSet(b *testing.B) {
	c := newCache(&cacheConfig{size: 1024 * 1024})
	req, resp := newBenchMsg()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.set(req, resp, benchUpstream, benchLogger)
	}
}

func BenchmarkCacheGet(b *testing.B) {
	c := newCache(&cacheConfig{size: 1024 * 1024})
	req, resp := newBenchMsg()

	c.set(req, resp, benchUpstream, benchLogger)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = c.get(req)
	}
}

func BenchmarkCacheSetParallel(b *testing.B) {
	c := newCache(&cacheConfig{size: 1024 * 1024})
	req, resp := newBenchMsg()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.set(req, resp, benchUpstream, benchLogger)
		}
	})
}

func BenchmarkCacheGetParallel(b *testing.B) {
	c := newCache(&cacheConfig{size: 1024 * 1024})
	req, resp := newBenchMsg()

	c.set(req, resp, benchUpstream, benchLogger)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _, _ = c.get(req)
		}
	})
}

func BenchmarkCacheItemPack(b *testing.B) {
	_, resp := newBenchMsg()

	c := &cache{}
	item := c.respToItem(resp, benchUpstream, benchLogger)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = item.pack()
	}
}

func BenchmarkCacheItemUnpack(b *testing.B) {
	req, resp := newBenchMsg()

	c := &cache{optimistic: false}
	item := c.respToItem(resp, benchUpstream, benchLogger)
	packed := item.pack()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.unpackItem(packed, req)
	}
}

func BenchmarkCacheGetWithSubnet(b *testing.B) {
	c := newCache(&cacheConfig{size: 1024 * 1024, withECS: true})
	req, resp := newBenchMsg()
	ip := &net.IPNet{IP: net.IP{192, 168, 1, 0}, Mask: net.CIDRMask(24, 32)}

	c.setWithSubnet(req, resp, benchUpstream, ip, benchLogger)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = c.getWithSubnet(req, ip)
	}
}

func BenchmarkCacheGetWithSubnetParallel(b *testing.B) {
	c := newCache(&cacheConfig{size: 1024 * 1024, withECS: true})
	req, resp := newBenchMsg()
	ip := &net.IPNet{IP: net.IP{192, 168, 1, 0}, Mask: net.CIDRMask(24, 32)}

	c.setWithSubnet(req, resp, benchUpstream, ip, benchLogger)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _, _ = c.getWithSubnet(req, ip)
		}
	})
}

func BenchmarkFilterRRSlice(b *testing.B) {
	rrs := []dns.RR{
		&dns.A{
			Hdr: dns.RR_Header{Name: "google.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300},
			A:   net.IP{8, 8, 8, 8},
		},
		&dns.AAAA{
			Hdr:  dns.RR_Header{Name: "google.com.", Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 300},
			AAAA: net.IP{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = filterRRSlice(rrs, true, 0, dns.TypeA)
	}
}
