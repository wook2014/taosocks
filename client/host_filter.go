package main

import (
	"bufio"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// ProxyType is
type ProxyType byte

const (
	proxyTypeNone       ProxyType = iota
	proxyTypeDirect               // direct, from rules.txt
	proxyTypeProxy                // proxy, from rules.txt
	proxyTypeReject               // reject, from rules.txt
	proxyTypeAutoDirect           // direct, from checker, auto-generated
	proxyTypeAutoProxy            // proxy, from checker, auto-generated
)

func (t ProxyType) String() string {
	switch t {
	case proxyTypeDirect:
		return "direct"
	case proxyTypeProxy:
		return "proxy"
	case proxyTypeReject:
		return "reject"
	case proxyTypeAutoDirect:
		return "auto-direct"
	case proxyTypeAutoProxy:
		return "auto-proxy"
	default:
		return "unknown"
	}
}

// IsAuto returns true if the rule is an auto-generated rule.
func (t ProxyType) IsAuto() bool {
	switch t {
	case proxyTypeAutoDirect, proxyTypeAutoProxy:
		return true
	default:
		return false
	}
}

func (t ProxyType) MarshalYAML() (interface{}, error) {
	return t.String(), nil
}

func (t *ProxyType) UnmarshalYAML(value *yaml.Node) error {
	var s string
	err := value.Decode(&s)
	if err != nil {
		return err
	}
	*t = ProxyTypeFromString(s)
	return nil
}

// ProxyTypeFromString is
func ProxyTypeFromString(name string) ProxyType {
	switch name {
	case "direct":
		return proxyTypeDirect
	case "proxy":
		return proxyTypeProxy
	case "reject":
		return proxyTypeReject
	case "auto-direct":
		return proxyTypeAutoDirect
	case "auto-proxy":
		return proxyTypeAutoProxy
	default:
		return proxyTypeNone
	}
}

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

// HostEntry holds data of a host proxy info.
type HostEntry struct {
	Type ProxyType `yaml:"type"` // proxy type for this host
	Port int       `yaml:"port"`
}

// HostFilter returns the proxy type on specified host.
type HostFilter struct {
	mu    sync.RWMutex
	hosts map[string]HostEntry
	cidrs map[*net.IPNet]HostEntry
}

// SaveAuto saves auto-generated rules.
func (f *HostFilter) SaveAuto(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	file, err := os.Create(path)
	if err != nil {
		return
	}
	defer file.Close()

	w := bufio.NewWriter(file)

	hosts := make(map[string]HostEntry)
	for host, entry := range f.hosts {
		if entry.Type.IsAuto() {
			hosts[host] = entry
		}
	}

	yaml.NewEncoder(w).Encode(hosts)

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

	hosts := make(map[string]HostEntry)
	yaml.NewDecoder(file).Decode(&hosts)

	for host, entry := range hosts {
		f.hosts[host] = entry
	}
}

// Init loads user-defined rules.
func (f *HostFilter) Init(path string) {
	f.hosts = make(map[string]HostEntry)
	f.cidrs = make(map[*net.IPNet]HostEntry)

	if file, err := os.Open(path); err != nil {
		tslog.Red("rule file not found: %s", path)
	} else {
		f.scanFile(file)
		file.Close()
	}

	go func() {
		// recheck every time client restarts
		time.Sleep(time.Second * 10)
		f.recheck()

		// then every 12 hours do a check
		for range time.Tick(time.Hour * 12) {
			f.recheck()
		}
	}()
}

func (f *HostFilter) recheck() {
	hosts := make(map[string]HostEntry)

	f.mu.RLock()
	for host, entry := range f.hosts {
		if entry.Type == proxyTypeAutoProxy {
			hosts[host] = entry
		}
	}
	f.mu.RUnlock()

	for host, entry := range hosts {
		tslog.Green("* Rechecking %s ...", host)
		if tcpChecker.Check(host, entry.Port) {
			f.AddHost(host, entry.Port, proxyTypeAutoDirect)
		}
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
			ptype := ProxyTypeFromString(toks[1])
			if ptype == proxyTypeNone {
				tslog.Red("invalid proxy type: %s", toks[1])
				continue
			}

			if strings.IndexByte(toks[0], '/') == -1 {
				f.hosts[toks[0]] = HostEntry{
					Type: ptype,
				}
			} else {
				_, ipnet, err := net.ParseCIDR(toks[0])
				if err == nil {
					f.cidrs[ipnet] = HostEntry{
						Type: ptype,
					}
				} else {
					tslog.Red("bad cidr: %s", toks[0])
				}
			}
		} else {
			tslog.Red("invalid rule: %s", rule)
		}
	}
}

// AddHost adds a rule. (thread-safe)
func (f *HostFilter) AddHost(host string, port int, ptype ProxyType) {
	f.mu.Lock()
	defer f.mu.Unlock()
	he, ok := f.hosts[host]
	f.hosts[host] = HostEntry{
		Type: ptype,
		Port: port,
	}
	if !ok {
		tslog.Green("+ Add Rule [%s] %s", ptype, host)
	} else {
		if he.Type != ptype {
			tslog.Green("* Change Rule [%s -> %s] %s", he.Type, ptype, host)
		}
	}
}

// DeleteHost deletes a rule. (thread-safe)
func (f *HostFilter) DeleteHost(host string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.hosts, host)
	tslog.Red("- Delete Rule %s", host)
}

// Test returns proxy type for host host.
func (f *HostFilter) Test(host string, port int) (proxyType ProxyType) {
	defer func() {
		if proxyType == proxyTypeNone {
			pty := proxyTypeAutoDirect
			tslog.Red("? checking %s ...", host)
			if !tcpChecker.Check(host, port) {
				pty = proxyTypeAutoProxy
			}
			f.AddHost(host, port, pty)
			proxyType = pty
		}
	}()

	f.mu.RLock()
	defer f.mu.RUnlock()

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
		if he, ok := f.hosts[host]; ok {
			return he.Type
		}
		ip := net.ParseIP(host)
		for ipnet, he := range f.cidrs {
			if ipnet.Contains(ip) {
				return he.Type
			}
		}
	} else if aty == Domain {
		// full match
		if he, ok := f.hosts[host]; ok {
			return he.Type
		}

		// test suffixes (sub strings)
		// eg. host is play.golang.org, then these suffixes will be tested:
		//		play.golang.org
		//		golang.org
		//		org
		part := host // don't modify host, it is used in defer
		for {
			index := strings.IndexByte(part, '.')
			if index == -1 {
				break
			}
			part = part[index+1:]
			if he, ok := f.hosts[part]; ok {
				// don't apply auto rules to suffix tests
				if !he.Type.IsAuto() {
					return he.Type
				}
			}
		}
	}

	return proxyTypeNone
}
