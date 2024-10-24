package main

import (
	"log"

	"github.com/miekg/dns"
)

func main() {
	log.SetFlags(11)
	dns.NewServeMux()
	dns.HandleFunc(".", func(w dns.ResponseWriter, req *dns.Msg) {
		log.Println(req)
		m := &dns.Msg{}
		m.SetReply(req)

		log.Println(req.Question[0].Name)

		m.Answer = append(m.Answer, &dns.A{})

		m.Extra = make([]dns.RR, 1)
		m.Extra[0] = &dns.TXT{Hdr: dns.RR_Header{Name: m.Question[0].Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 0}, Txt: []string{"Hello world"}}
		w.WriteMsg(m)
	})
	err := dns.ListenAndServe(":2053", "udp", dns.DefaultServeMux)

	log.Println("Stop", err)
}
