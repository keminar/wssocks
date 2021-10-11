package wss

import (
	"bytes"
	"net"
	"strings"
)

/**
10.0.0.0-10.255.255.255
172.16.0.0-172.31.255.255
192.168.0.0-192.168.255.255
上面这三个地址范围都是私有地址
todo ipv6有没有私有地址段
*/

type ipBlock struct {
	ip1 net.IP
	ip2 net.IP
}

var priviteIps []ipBlock

func init() {
	a := ipBlock{net.ParseIP("10.0.0.0"), net.ParseIP("10.255.255.255")}
	b := ipBlock{net.ParseIP("172.16.0.0"), net.ParseIP("172.31.255.255")}
	c := ipBlock{net.ParseIP("192.168.0.0"), net.ParseIP("192.168.255.255")}
	priviteIps = append(priviteIps, a, b, c)
}

// 检查域名是否私有地址
func checkAddrPrivite(addr string) bool {
	addrs := strings.Split(addr, ":")
	upIPs, _ := net.LookupIP(addrs[0])
	if len(upIPs) > 0 {
		for _, ip := range upIPs {
			//fmt.Println("addr", addr, ip.String())
			if checkIpPrivite(ip.String()) {
				return true
			}
		}
	}
	return false
}

// 检查ip是否私有地址
func checkIpPrivite(ip string) bool {
	if ip == "127.0.0.1" {
		return true
	}
	trial := net.ParseIP(ip)
	if trial.To4() == nil {
		//fmt.Printf("%v is not an IPv4 address\n", trial)
		return false
	}
	for _, p := range priviteIps {
		if bytes.Compare(trial, p.ip1) >= 0 && bytes.Compare(trial, p.ip2) <= 0 {
			//fmt.Printf("%v is between %v and %v\n", trial, p.ip1, p.ip2)
			return true
		}
		//fmt.Printf("%v is NOT between %v and %v\n", trial, p.ip1, p.ip2)
	}
	return false
}
