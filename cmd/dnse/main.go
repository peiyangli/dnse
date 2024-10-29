package main

import (
	"flag"
	"log"
	"net"

	"github.com/miekg/dns"
)

type Flags struct {
	Addr string
}

var gFlags Flags

func Init() {
	flag.StringVar(&gFlags.Addr, "addr", ":53", "ip:port")
	flag.Parse()
}

func main() {
	log.SetFlags(11)
	Init()

	log.Println(gFlags)

	dns.NewServeMux()
	dns.HandleFunc(".", func(w dns.ResponseWriter, req *dns.Msg) {
		log.Println(req)
		m := &dns.Msg{}
		m.SetReply(req)

		log.Println(req.Question[0].Name)

		m.Answer = append(m.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: req.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 0},
			A:   net.ParseIP("127.0.0.1"),
		})

		m.Answer = append(m.Answer, &dns.TXT{
			Hdr: dns.RR_Header{Name: req.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 0},
			Txt: []string{"world"},
		})

		m.Extra = append(m.Extra, &dns.TXT{Hdr: dns.RR_Header{Name: m.Question[0].Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 0}, Txt: []string{"Hello world"}})
		w.WriteMsg(m)
	})

	err := dns.ListenAndServe(gFlags.Addr, "udp", dns.DefaultServeMux)

	log.Println("Stop", err)
}
