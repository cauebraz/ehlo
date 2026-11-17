// Package provider maps a recipient domain to the mailbox provider that
// runs its MX, so metrics and warmup lanes group by who filters the mail.
package provider

import (
	"slices"
	"sort"
	"strings"
)

// Other is the provider of every domain no rule names.
const Other = "other"

// Rule names a provider and the domains or domain suffixes it serves. A
// suffix starts with a dot and matches the domain and every subdomain.
type Rule struct {
	Name    string   `yaml:"name" json:"name"`
	Domains []string `yaml:"domains" json:"domains"`
}

// Classifier maps recipient domains to provider names. It is immutable
// after New.
type Classifier struct {
	exact  map[string]string
	suffix []suffixRule
	names  []string
}

type suffixRule struct {
	suffix, name string
}

// Default covers the providers that publish sender rules or reply codes.
// A domain maps to the company that runs its mailboxes, which is who
// decides whether the mail gets in. Microsoft 365 and Google Workspace
// tenants use their own domains and cannot be told apart here; they fall
// under other.
var Default = []Rule{
	{"gmail", []string{"gmail.com", "googlemail.com", ".google.com"}},
	{"outlook", []string{
		"outlook.com", "hotmail.com", "live.com", "msn.com", "passport.com",
		".outlook.com", ".hotmail.com", ".live.com",
		"outlook.com.br", "outlook.fr", "outlook.de", "outlook.es", "outlook.it", "outlook.jp", "outlook.co.uk", "outlook.be", "outlook.at", "outlook.ie", "outlook.in", "outlook.ph", "outlook.sa",
		"hotmail.co.uk", "hotmail.fr", "hotmail.de", "hotmail.es", "hotmail.it", "hotmail.com.br", "hotmail.co.jp", "hotmail.ca", "hotmail.be", "hotmail.nl", "hotmail.se",
		"live.co.uk", "live.fr", "live.de", "live.nl", "live.com.au", "live.ca", "live.jp", "live.it", "live.se", "live.dk", "live.be",
	}},
	{"yahoo", []string{
		"yahoo.com", "ymail.com", "rocketmail.com", ".yahoo.com",
		"yahoo.co.uk", "yahoo.com.br", "yahoo.fr", "yahoo.de", "yahoo.es", "yahoo.it", "yahoo.ca", "yahoo.com.au", "yahoo.co.jp", "yahoo.in", "yahoo.co.in", "yahoo.com.mx", "yahoo.com.ar", "yahoo.co.id", "yahoo.com.sg", "yahoo.com.ph", "yahoo.com.vn", "yahoo.com.tw", "yahoo.com.hk", "yahoo.gr", "yahoo.se", "yahoo.dk", "yahoo.no", "yahoo.ro", "yahoo.co.nz", "yahoo.ie", "yahoo.pl", "yahoo.at", "yahoo.ch", "yahoo.be", "yahoo.nl", "yahoo.cl", "yahoo.com.co", "yahoo.com.pe", "yahoo.com.ve",
		"aol.com", ".aol.com", "aol.co.uk", "aol.de", "aol.fr", "aim.com", "wow.com", "netscape.net", "cs.com", "compuserve.com",
		"verizon.net", "att.net", "sbcglobal.net", "bellsouth.net", "ameritech.net", "pacbell.net", "swbell.net", "prodigy.net", "nvbell.net", "snet.net", "flash.net", "currently.com",
		"sky.com", "btinternet.com", "frontier.com", "frontiernet.net",
	}},
	{"icloud", []string{"icloud.com", "me.com", "mac.com", ".icloud.com"}},
	{"comcast", []string{"comcast.net", "xfinity.com"}},
	{"spectrum", []string{"charter.net", "rr.com", ".rr.com", "twc.com", "roadrunner.com", "brighthouse.com", "bhn.net"}},
	{"cox", []string{"cox.net"}},
	{"orange", []string{"orange.fr", "wanadoo.fr"}},
	{"sfr", []string{"sfr.fr", "neuf.fr", "cegetel.net", "club-internet.fr", "numericable.fr"}},
	{"free", []string{"free.fr", "aliceadsl.fr"}},
	{"laposte", []string{"laposte.net"}},
	{"gmx", []string{"gmx.net", "gmx.de", "gmx.at", "gmx.ch", "gmx.com", "gmx.co.uk", "gmx.fr", "gmx.es", "gmx.it", "gmx.us", "web.de", "mail.com"}},
	{"t-online", []string{"t-online.de", "magenta.de"}},
	{"libero", []string{"libero.it", "virgilio.it", "iol.it", "inwind.it", "blu.it", "giallo.it"}},
	{"mail.ru", []string{"mail.ru", "bk.ru", "inbox.ru", "list.ru", "internet.ru", "vk.com"}},
	{"yandex", []string{"yandex.ru", "yandex.com", "ya.ru", "yandex.ua", "yandex.kz", "yandex.by", "narod.ru"}},
	{"seznam", []string{"seznam.cz", "email.cz", "post.cz", "spoluzaci.cz"}},
	{"proton", []string{"proton.me", "protonmail.com", "protonmail.ch", "pm.me"}},
	{"fastmail", []string{"fastmail.com", "fastmail.fm", "messagingengine.com"}},
	{"zoho", []string{"zoho.com", "zohomail.com"}},
	{"qq", []string{"qq.com", "foxmail.com", "vip.qq.com"}},
	{"netease", []string{"163.com", "126.com", "yeah.net", "188.com", "vip.163.com"}},
	{"naver", []string{"naver.com"}},
	{"kakao", []string{"daum.net", "hanmail.net", "kakao.com"}},
	{"uol", []string{"uol.com.br", "bol.com.br"}},
	{"terra", []string{"terra.com.br", "terra.es"}},
	{"movistar", []string{"movistar.es", "telefonica.net"}},
}

// New compiles the rules. Exact domains win over suffixes, and the longest
// suffix wins among suffixes. When two rules list the same domain or suffix,
// the later rule wins.
func New(rules []Rule) *Classifier {
	c := &Classifier{exact: map[string]string{}}
	seen := map[string]bool{}
	suffixes := map[string]string{}
	for _, r := range rules {
		name := strings.ToLower(strings.TrimSpace(r.Name))
		if name == "" {
			continue
		}
		if !seen[name] {
			seen[name] = true
			c.names = append(c.names, name)
		}
		for _, d := range r.Domains {
			d = strings.ToLower(strings.TrimSpace(d))
			switch {
			case d == "":
			case strings.HasPrefix(d, "."):
				suffixes[d] = name
			default:
				c.exact[d] = name
			}
		}
	}
	for d, name := range suffixes {
		c.suffix = append(c.suffix, suffixRule{d, name})
	}
	sort.Slice(c.suffix, func(i, j int) bool {
		if len(c.suffix[i].suffix) != len(c.suffix[j].suffix) {
			return len(c.suffix[i].suffix) > len(c.suffix[j].suffix)
		}
		return c.suffix[i].suffix < c.suffix[j].suffix
	})
	if !seen[Other] {
		c.names = append(c.names, Other)
	}
	return c
}

// Classify returns the provider for a recipient domain, or Other.
func (c *Classifier) Classify(domain string) string {
	d := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	if d == "" {
		return Other
	}
	if name, ok := c.exact[d]; ok {
		return name
	}
	for _, s := range c.suffix {
		if strings.HasSuffix(d, s.suffix) || d == s.suffix[1:] {
			return s.name
		}
	}
	return Other
}

// Known reports whether name is a provider the classifier can return.
func (c *Classifier) Known(name string) bool {
	return slices.Contains(c.names, name)
}

// Names lists every provider the classifier can return, Other last.
func (c *Classifier) Names() []string {
	return append([]string(nil), c.names...)
}
