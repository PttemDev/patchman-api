package servicemap

import (
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Endpoint is one address found in configuration. Never carries userinfo,
// path, query or any other part of the original value.
type Endpoint struct {
	Scheme string
	Host   string // lower-case, no brackets
	Port   int    // 0 = unknown
}

func (e Endpoint) HostPort() string {
	h := e.Host
	if strings.Contains(h, ":") {
		h = "[" + h + "]"
	}
	if e.Port > 0 {
		return h + ":" + strconv.Itoa(e.Port)
	}
	return h
}

// String is the evidence form: scheme://host:port.
func (e Endpoint) String() string {
	if e.Scheme != "" {
		return e.Scheme + "://" + e.HostPort()
	}
	return e.HostPort()
}

var (
	urlRe      = regexp.MustCompile(`(?i)\b(jdbc:)?([a-z][a-z0-9+.-]{1,30})://([^\s'"<>,;]+(?:,[^\s'"<>,;/]+)*)`)
	hostPortRe = regexp.MustCompile(`(?i)(?:^|[\s,;=('"\[])((?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?|\d{1,3}(?:\.\d{1,3}){3}):(\d{2,5})\b`)
	hostOnlyRe = regexp.MustCompile(`(?i)^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
)

// hostKeySuffixes mark an env key whose bare value is a host name.
var hostKeySuffixes = []string{"_HOST", "_HOSTNAME", "_SERVER", "_SERVERS", "_ADDR", "_ADDRESS", "_ENDPOINT", "_BROKERS", "_BOOTSTRAP_SERVERS", "_NODES"}

// ExtractEndpoints finds network addresses in a configuration value. key is
// the env var / ConfigMap key (used to recognise bare host names such as
// DB_HOST=postgres). siblingPort resolves "<PREFIX>_PORT" for such keys.
func ExtractEndpoints(key, value string, siblingPort func(string) int) []Endpoint {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64<<10 {
		return nil
	}
	var out []Endpoint
	seen := map[string]bool{}
	add := func(e Endpoint) {
		e.Host = strings.ToLower(strings.Trim(e.Host, "[]."))
		if !usableHost(e.Host) {
			return
		}
		k := e.String()
		if !seen[k] {
			seen[k] = true
			out = append(out, e)
		}
	}
	spans := [][2]int{}
	for _, m := range urlRe.FindAllStringSubmatchIndex(value, -1) {
		spans = append(spans, [2]int{m[0], m[1]})
		scheme := strings.ToLower(value[m[4]:m[5]])
		rest := value[m[6]:m[7]]
		for _, e := range parseURLHosts(scheme, rest) {
			add(e)
		}
	}
	// host:port outside URLs ("redis:6379", "kafka-0:9092,kafka-1:9092").
	masked := []byte(value)
	for _, s := range spans {
		for i := s[0]; i < s[1]; i++ {
			masked[i] = ' '
		}
	}
	for _, m := range hostPortRe.FindAllStringSubmatch(string(masked), -1) {
		host := m[1]
		if isNumber(host) {
			continue // "10:30" is a time, not an address
		}
		port, _ := strconv.Atoi(m[2])
		if port <= 0 || port > 65535 {
			continue
		}
		add(Endpoint{Host: host, Port: port})
	}
	// A bare host in a host-named key: DB_HOST=postgres, REDIS_ADDR=cache.
	if len(out) == 0 && key != "" {
		up := strings.ToUpper(key)
		for _, suf := range hostKeySuffixes {
			if strings.HasSuffix(up, suf) {
				for _, h := range strings.Split(value, ",") {
					h = strings.TrimSpace(h)
					if hostOnlyRe.MatchString(h) && !isNumber(h) {
						port := 0
						if siblingPort != nil {
							port = siblingPort(strings.TrimSuffix(up, suf))
						}
						add(Endpoint{Host: h, Port: port, Scheme: schemeFromKey(up)})
					}
				}
				break
			}
		}
	}
	return out
}

// parseURLHosts handles single and multi-host URLs
// (mongodb://a:27017,b:27017/db, postgres://u:p@h:5432/db).
func parseURLHosts(scheme, rest string) []Endpoint {
	scheme = normalizeScheme(scheme)
	// Drop userinfo — it may hold a password and is never kept.
	if at := strings.LastIndex(strings.SplitN(rest, "/", 2)[0], "@"); at >= 0 {
		rest = rest[at+1:]
	}
	hostPart := rest
	if i := strings.IndexAny(hostPart, "/?#"); i >= 0 {
		hostPart = hostPart[:i]
	}
	var out []Endpoint
	for _, hp := range strings.Split(hostPart, ",") {
		hp = strings.TrimSpace(hp)
		if hp == "" {
			continue
		}
		u, err := url.Parse("x://" + hp)
		if err != nil || u.Hostname() == "" {
			continue
		}
		port, _ := strconv.Atoi(u.Port())
		if port == 0 {
			port = defaultPort(scheme)
		}
		out = append(out, Endpoint{Scheme: scheme, Host: u.Hostname(), Port: port})
	}
	return out
}

func normalizeScheme(s string) string {
	s = strings.ToLower(s)
	switch s {
	case "postgresql", "postgres", "pgsql":
		return "postgres"
	case "mongodb+srv":
		return "mongodb"
	case "rediss":
		return "redis"
	case "amqps":
		return "amqp"
	case "sqlserver", "mssql":
		return "sqlserver"
	}
	return s
}

func defaultPort(scheme string) int {
	switch scheme {
	case "http", "ws":
		return 80
	case "https", "wss":
		return 443
	case "postgres":
		return 5432
	case "mysql", "mariadb":
		return 3306
	case "sqlserver":
		return 1433
	case "mongodb":
		return 27017
	case "redis":
		return 6379
	case "amqp":
		return 5672
	case "nats":
		return 4222
	}
	return 0
}

func schemeFromKey(up string) string {
	switch {
	case strings.Contains(up, "POSTGRES") || strings.HasPrefix(up, "PG"):
		return "postgres"
	case strings.Contains(up, "MYSQL") || strings.Contains(up, "MARIADB"):
		return "mysql"
	case strings.Contains(up, "MONGO"):
		return "mongodb"
	case strings.Contains(up, "REDIS"):
		return "redis"
	case strings.Contains(up, "RABBIT") || strings.Contains(up, "AMQP"):
		return "amqp"
	case strings.Contains(up, "KAFKA"):
		return "kafka"
	case strings.Contains(up, "ELASTIC") || strings.Contains(up, "OPENSEARCH"):
		return "elasticsearch"
	}
	return ""
}

func usableHost(h string) bool {
	if h == "" || h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return false
	}
	if ip := net.ParseIP(h); ip != nil {
		return !ip.IsLoopback() && !ip.IsUnspecified()
	}
	// Bare words that are clearly not hosts.
	switch h {
	case "true", "false", "null", "none", "yes", "no", "on", "off":
		return false
	}
	return true
}

func isNumber(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}

// Subtype classifies an endpoint by scheme, then port.
func Subtype(scheme string, port int) string {
	switch normalizeScheme(scheme) {
	case "postgres":
		return "postgres"
	case "mysql", "mariadb":
		return "mysql"
	case "sqlserver":
		return "mssql"
	case "mongodb":
		return "mongo"
	case "redis":
		return "redis"
	case "amqp":
		return "rabbitmq"
	case "kafka":
		return "kafka"
	case "nats":
		return "nats"
	case "elasticsearch":
		return "elasticsearch"
	case "http", "https", "ws", "wss", "grpc":
		return "http"
	}
	return subtypeByPort(port)
}

func subtypeByPort(port int) string {
	switch port {
	case 5432, 6432:
		return "postgres"
	case 3306:
		return "mysql"
	case 1433:
		return "mssql"
	case 27017:
		return "mongo"
	case 6379, 6380, 26379:
		return "redis"
	case 5672, 5671, 15672:
		return "rabbitmq"
	case 9092, 9093, 9094:
		return "kafka"
	case 4222:
		return "nats"
	case 9200:
		return "elasticsearch"
	case 11211:
		return "memcached"
	case 9042:
		return "cassandra"
	case 8123:
		return "clickhouse"
	case 2181:
		return "zookeeper"
	case 80, 443, 8080, 8000, 8443, 3000, 5000, 9090:
		return "http"
	}
	return ""
}

// KindOf maps a subtype to a node kind.
func KindOf(subtype string) string {
	switch subtype {
	case "rabbitmq", "kafka", "nats":
		return KindMessaging
	case "postgres", "mysql", "mssql", "mongo", "redis", "elasticsearch", "memcached", "cassandra", "clickhouse", "s3", "zookeeper":
		return KindDatastore
	}
	return KindService
}

// ProtocolOf is the edge protocol label for a subtype.
func ProtocolOf(subtype string) string {
	switch subtype {
	case "rabbitmq":
		return "amqp"
	case "":
		return "tcp"
	}
	return subtype
}
