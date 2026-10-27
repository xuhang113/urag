package acquire

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// robotsRules 是某一主机对当前 User-Agent 生效的规则。
// disallowAll 表示 robots.txt 本身拒绝访问，整站按 robots 跳过。
type robotsRules struct {
	allow       []robotRule
	disallow    []robotRule
	delay       time.Duration
	disallowAll bool
}

type robotRule struct {
	pattern string
	re      *regexp.Regexp
}

// parseRobots 只采用与 ua 最匹配的一组规则；没有具体匹配时用 User-agent: *。
// 其他组的 Disallow 和 Crawl-delay 不生效。
func parseRobots(body, ua string) robotsRules {
	body = strings.TrimPrefix(body, "\uFEFF")
	ua = strings.ToLower(ua)
	type group struct {
		agents   []string
		allow    []robotRule
		disallow []robotRule
		delay    time.Duration
		hasRule  bool
	}
	var groups []*group
	var cur *group
	start := func() *group {
		g := &group{}
		groups = append(groups, g)
		return g
	}
	for _, rawLine := range strings.Split(body, "\n") {
		line := strings.TrimSpace(rawLine)
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		switch key {
		case "user-agent":
			if cur == nil || cur.hasRule {
				cur = start()
			}
			cur.agents = append(cur.agents, strings.ToLower(val))
		case "allow":
			if cur == nil {
				cur = start()
			}
			cur.hasRule = true
			if val != "" {
				cur.allow = append(cur.allow, compileRobotRule(val))
			}
		case "disallow":
			if cur == nil {
				cur = start()
			}
			cur.hasRule = true
			if val != "" {
				cur.disallow = append(cur.disallow, compileRobotRule(val))
			}
		case "crawl-delay":
			if cur == nil {
				cur = start()
			}
			cur.hasRule = true
			sec, err := strconv.ParseFloat(val, 64)
			if err == nil && sec > 0 {
				cur.delay = time.Duration(sec * float64(time.Second))
			}
		}
	}

	var star *group
	var best *group
	bestLen := -1
	for _, g := range groups {
		for _, agent := range g.agents {
			if agent == "*" {
				if star == nil {
					star = g
				}
				continue
			}
			if agent != "" && strings.Contains(ua, agent) && len(agent) > bestLen {
				best = g
				bestLen = len(agent)
			}
		}
	}
	chosen := best
	if chosen == nil {
		chosen = star
	}
	if chosen == nil {
		return robotsRules{}
	}
	return robotsRules{allow: chosen.allow, disallow: chosen.disallow, delay: chosen.delay}
}

func compileRobotRule(pattern string) robotRule {
	rule := robotRule{pattern: pattern}
	if !strings.Contains(pattern, "*") && !strings.HasSuffix(pattern, "$") {
		return rule
	}
	anchored := strings.HasSuffix(pattern, "$")
	body := pattern
	if anchored {
		body = strings.TrimSuffix(body, "$")
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(body); i++ {
		if body[i] == '*' {
			b.WriteString(".*")
			continue
		}
		b.WriteString(regexp.QuoteMeta(body[i : i+1]))
	}
	if anchored {
		b.WriteString("$")
	}
	rule.re = regexp.MustCompile(b.String())
	return rule
}

// robotsDisallow 在 Disallow 匹配且没有更长或同样长的 Allow 时返回 true。
func robotsDisallow(u *url.URL, rules robotsRules) bool {
	if u == nil {
		return false
	}
	if rules.disallowAll {
		return true
	}
	allowLen := -1
	denyLen := -1
	for _, rule := range rules.allow {
		if n, ok := rule.match(robotsTarget(u, rule.pattern)); ok && n > allowLen {
			allowLen = n
		}
	}
	for _, rule := range rules.disallow {
		if n, ok := rule.match(robotsTarget(u, rule.pattern)); ok && n > denyLen {
			denyLen = n
		}
	}
	if denyLen < 0 {
		return false
	}
	if allowLen >= denyLen {
		return false
	}
	return true
}

func (r robotRule) match(path string) (int, bool) {
	if r.pattern == "" {
		return 0, false
	}
	if r.re != nil {
		if r.re.MatchString(path) {
			return len(r.pattern), true
		}
		return 0, false
	}
	if strings.HasPrefix(path, r.pattern) {
		return len(r.pattern), true
	}
	return 0, false
}

func robotsTarget(u *url.URL, pattern string) string {
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	if strings.Contains(pattern, "?") && u.RawQuery != "" {
		return p + "?" + u.RawQuery
	}
	return p
}
