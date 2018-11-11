package main

import (
	"bufio"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
)

// ProxyType is
type ProxyType byte

const (
	_                   ProxyType = iota // No rules applied
	proxyTypeDirect                      // direct, from rules.txt
	proxyTypeProxy                       // proxy, from rules.txt
	proxyTypeReject                      // reject, from rules.txt
	proxyTypeAutoDirect                  // direct, from checker, auto-generated
	proxyTypeAutoProxy                   // proxy, from checker, auto-generated
)

// AddrType is
type AddrType uint

// Address Types
const (
	_ AddrType = iota
	IPv4
	Domain
)

var reIsComment = regexp.MustCompile(`^[ \t]*#`)

func isComment(line string) bool {
	return reIsComment.MatchString(line)
}

// AutoRule is an auto-generated rule.
type AutoRule struct {
	add   bool
	host  string
	ptype ProxyType
}

// HostFilter returns the proxy type on specified host.
type HostFilter struct {
	ch    chan AutoRule
	hosts map[string]ProxyType
	cidrs map[*net.IPNet]ProxyType
}

// SaveAuto saves auto-generated rules.
func (f *HostFilter) SaveAuto(path string) {
	file, err := os.Create(path)
	if err != nil {
		return
	}
	defer file.Close()

	w := bufio.NewWriter(file)

	for k, t := range f.hosts {
		switch t {
		case proxyTypeAutoDirect, proxyTypeAutoProxy:
			w.WriteString(k)
			switch t {
			case proxyTypeAutoDirect:
				w.WriteString(",auto-direct")
			case proxyTypeAutoProxy:
				w.WriteString(",auto-proxy")
			}
			w.WriteByte('\n')
		}
	}

	w.Flush()
	file.Close()
}

// LoadAuto loads auto-generated rules.
func (f *HostFilter) LoadAuto(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}

	defer file.Close()

	f.scanFile(file)
}

// Init loads user-difined rules.
func (f *HostFilter) Init(path string) {
	f.ch = make(chan AutoRule)
	f.hosts = make(map[string]ProxyType)
	f.cidrs = make(map[*net.IPNet]ProxyType)

	go f.opRoutine()

	if file, err := os.Open(path); err != nil {
		tslog.Red("rule file not found: %s\n", path)
	} else {
		f.scanFile(file)
		file.Close()
	}
}

func (f *HostFilter) scanFile(reader io.Reader) {
	scanner := bufio.NewScanner(reader)

	for scanner.Scan() {
		rule := strings.Trim(scanner.Text(), " \t")
		if isComment(rule) || rule == "" {
			continue
		}
		toks := strings.Split(rule, ",")
		if len(toks) == 2 {
			var ty ProxyType
			switch toks[1] {
			case "direct":
				ty = proxyTypeDirect
			case "proxy":
				ty = proxyTypeProxy
			case "reject":
				ty = proxyTypeReject
			case "auto-direct":
				ty = proxyTypeAutoDirect
			case "auto-proxy":
				ty = proxyTypeAutoProxy
			default:
				tslog.Red("invalid proxy type: %s\n", toks[1])
				continue
			}

			if strings.IndexByte(toks[0], '/') == -1 {
				f.hosts[toks[0]] = ty
			} else {
				_, ipnet, err := net.ParseCIDR(toks[0])
				if err == nil {
					f.cidrs[ipnet] = ty
				} else {
					tslog.Red("bad cidr: %s\n", toks[0])
				}
			}
		} else {
			tslog.Red("invalid rule: %s\n", rule)
		}
	}
}

// AddHost adds a rule. (thread-safe)
func (f *HostFilter) AddHost(host string, ptype ProxyType) {
	f.ch <- AutoRule{
		add:   true,
		host:  host,
		ptype: ptype,
	}
}

// DeleteHost deletes a rule. (thread-safe)
func (f *HostFilter) DeleteHost(host string) {
	f.ch <- AutoRule{
		add:  false,
		host: host,
	}
}

func (f *HostFilter) opRoutine() {
	for {
		s := <-f.ch
		switch s.add {
		case true:
			if _, ok := f.hosts[s.host]; !ok {
				f.hosts[s.host] = s.ptype
				switch s.ptype {
				case proxyTypeAutoDirect, proxyTypeAutoProxy:
					tslog.Green("+ 添加规则：%s", s.host)
				}
			}
		case false:
			delete(f.hosts, s.host)
			tslog.Red("- 删除规则：%s", s.host)
		}
	}
}

// Test returns proxy type for host host.
func (f *HostFilter) Test(host string, port string) ProxyType {
	host = strings.ToLower(host)

	// if host is TopLevel, like localhost.
	if !strings.Contains(host, ".") {
		return proxyTypeDirect
	}

	aty := Domain
	if net.ParseIP(host).To4() != nil {
		aty = IPv4
	}

	if aty == IPv4 {
		if ty, ok := f.hosts[host]; ok {
			return ty
		}
		ip := net.ParseIP(host)
		for ipnet, ty := range f.cidrs {
			if ipnet.Contains(ip) {
				return ty
			}
		}
	} else if aty == Domain {
		// test suffixes (sub strings)
		// eg. host is play.golang.org, then these suffixes will be tested:
		//		play.golang.org
		//		golang.org
		//		org
		part := host
		for {
			if ty, ok := f.hosts[part]; ok {
				return ty
			}
			index := strings.IndexByte(part, '.')
			if index == -1 {
				break
			}
			part = part[index+1:]
		}
	}

	pty := proxyTypeAutoDirect
	if !tcpChecker.Check(host, port) {
		pty = proxyTypeAutoProxy
	}
	f.AddHost(host, pty)
	return pty
}
