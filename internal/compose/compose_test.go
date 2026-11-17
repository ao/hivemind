package compose

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

// env is a Lookup over a fixed map.
func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// web returns a file with one service, web, running nginx:1.27, with extra
// indented under it.
func web(extra string) string {
	s := "services:\n  web:\n    image: nginx:1.27\n"
	for _, line := range strings.Split(strings.TrimRight(extra, "\n"), "\n") {
		if line != "" {
			s += "    " + line + "\n"
		}
	}
	return s
}

func base() spec.App { return spec.App{Name: "web", Image: "nginx:1.27"} }

func dur(s string) spec.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		panic(err)
	}
	return spec.Duration(d)
}

func js(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestConvertSupported converts each supported key and compares the spec,
// normalised, with the one expected.
func TestConvertSupported(t *testing.T) {
	vars := map[string]string{"TAG": "1.27", "EMPTY": "", "PORT": "9090", "NAME": "world", "SPACE": "a b"}
	cases := []struct {
		name  string
		file  string
		vars  map[string]string
		edit  func(*spec.App)
		notes []string
	}{
		{name: "image only", file: web("")},
		{name: "command string", file: web(`command: sh -c 'echo "a b"; sleep 1'`),
			edit: func(a *spec.App) { a.Command = []string{"sh", "-c", `echo "a b"; sleep 1`} }},
		{name: "command string with escapes", file: web(`command: 'echo a\ b "c \"d\"" \$$x'`),
			edit: func(a *spec.App) { a.Command = []string{"echo", "a b", `c "d"`, "$x"} }},
		{name: "command list", file: web("command: [sleep, 3600, true]"),
			edit: func(a *spec.App) { a.Command = []string{"sleep", "3600", "true"} }},
		{name: "command interpolated, then split", file: web(`command: echo ${SPACE} "${NAME}"`),
			edit: func(a *spec.App) { a.Command = []string{"echo", "a", "b", "world"} }},
		{name: "command null", file: web("command: null")},
		{name: "environment map", file: web("environment:\n  A: x\n  B: 8080\n  C: true\n  D: 1.50\n  E: ''\n  F: 0x10\n  G: 'it''s'\n  H: ${NAME}"),
			edit: func(a *spec.App) {
				a.Env = map[string]string{"A": "x", "B": "8080", "C": "true", "D": "1.5", "E": "", "F": "16", "G": "it's", "H": "world"}
			}},
		{name: "environment list", file: web("environment:\n  - A=x=y\n  - B=\n  - C=${NAME}\n  - ${NAME}_KEY=v"),
			edit: func(a *spec.App) { a.Env = map[string]string{"A": "x=y", "B": "", "C": "world", "world_KEY": "v"} }},
		{name: "environment null", file: web("environment:")},
		{name: "port with no address", file: web(`ports: ["8080:80"]`),
			edit:  func(a *spec.App) { a.Ports = []spec.Port{{Bind: "127.0.0.1", Host: 8080, Container: 80}} },
			notes: []string{`services.web.ports[0]: published on 127.0.0.1:8080 only, the hivemind default; Compose would publish it on every interface. To publish elsewhere, write "0.0.0.0:8080:80" or another address`}},
		{name: "ports with addresses", file: web(`ports: ["0.0.0.0:8443:443", "[::1]:53:53/udp", "::1:54:54", "10.0.0.2:9000:9000/tcp"]`),
			edit: func(a *spec.App) {
				a.Ports = []spec.Port{{Bind: "0.0.0.0", Host: 8443, Container: 443}, {Bind: "::1", Host: 53, Container: 53, Protocol: "udp"},
					{Bind: "::1", Host: 54, Container: 54}, {Bind: "10.0.0.2", Host: 9000, Container: 9000}}
			}},
		{name: "port from a variable", file: web(`ports: ["127.0.0.1:${PORT}:80"]`),
			edit: func(a *spec.App) { a.Ports = []spec.Port{{Bind: "127.0.0.1", Host: 9090, Container: 80}} }},
		{name: "long port", file: web("ports:\n  - target: 80\n    published: \"8081\"\n    host_ip: 0.0.0.0\n    protocol: udp\n    mode: host\n    x-note: ignored"),
			edit: func(a *spec.App) {
				a.Ports = []spec.Port{{Bind: "0.0.0.0", Host: 8081, Container: 80, Protocol: "udp"}}
			}},
		{name: "long port with no address", file: web("ports:\n  - target: 80\n    published: ${PORT}"),
			edit:  func(a *spec.App) { a.Ports = []spec.Port{{Bind: "127.0.0.1", Host: 9090, Container: 80}} },
			notes: []string{"services.web.ports[0]: published on 127.0.0.1:9090 only, the hivemind default; Compose would publish it on every interface. To publish elsewhere, set host_ip to 0.0.0.0 or another address"}},
		{name: "long port with an empty address", file: web("ports: [{target: 80, published: 81, host_ip: ''}]"),
			edit:  func(a *spec.App) { a.Ports = []spec.Port{{Bind: "127.0.0.1", Host: 81, Container: 80}} },
			notes: []string{"services.web.ports[0]: published on 127.0.0.1:81 only, the hivemind default; Compose would publish it on every interface. To publish elsewhere, set host_ip to 0.0.0.0 or another address"}},
		{name: "volumes", file: "volumes:\n  data:\n  logs: {}\n  cache: {x-size: 1}\nservices:\n  web:\n    image: nginx:1.27\n    volumes:\n      - data:/var/lib/data\n      - logs:/var/log/app:rw\n      - type: volume\n        source: cache\n        target: /cache\n        read_only: false\n        x-why: ignored\n",
			edit: func(a *spec.App) {
				a.Volumes = []spec.Volume{{Name: "data", Path: "/var/lib/data"}, {Name: "logs", Path: "/var/log/app"}, {Name: "cache", Path: "/cache"}}
			}},
		{name: "deploy", file: web("deploy:\n  mode: replicated\n  replicas: 3\n  resources:\n    limits: {cpus: '0.5', memory: 512m}\n    reservations: {cpus: 0.50, memory: 512M}\n  restart_policy: {condition: any}\n  x-team: core"),
			edit: func(a *spec.App) {
				a.Replicas = 3
				a.Resources = spec.Resources{CPU: 0.5, Memory: 512 << 20}
			}},
		{name: "deploy with reservations left out", file: web("deploy:\n  resources:\n    limits: {cpus: 2, memory: 1gb}\n  restart_policy: {}"),
			edit: func(a *spec.App) { a.Resources = spec.Resources{CPU: 2, Memory: 1 << 30} }},
		{name: "replicas from a variable", file: web("deploy: {replicas: \"${PORT:-x}\"}"), vars: map[string]string{"PORT": "4"},
			edit: func(a *spec.App) { a.Replicas = 4 }},
		{name: "healthcheck CMD", file: web("healthcheck:\n  test: [CMD, curl, -f, 'http://localhost/']\n  interval: 1m30s\n  timeout: 10s\n  retries: 5\n  start_period: 40s\n  x-y: z"),
			edit: func(a *spec.App) {
				a.Health = &spec.Health{Exec: []string{"curl", "-f", "http://localhost/"}, Interval: dur("90s"), Timeout: dur("10s"), Retries: 5, StartPeriod: dur("40s")}
			}},
		{name: "healthcheck CMD-SHELL", file: web(`healthcheck: {test: [CMD-SHELL, "curl -f http://localhost/ || exit 1"]}`),
			edit: func(a *spec.App) {
				a.Health = &spec.Health{Exec: []string{"/bin/sh", "-c", "curl -f http://localhost/ || exit 1"}}
			}},
		{name: "healthcheck string", file: web(`healthcheck: {test: "pg_isready -U $$USER", interval: 5s, timeout: 1s}`),
			edit: func(a *spec.App) {
				a.Health = &spec.Health{Exec: []string{"/bin/sh", "-c", "pg_isready -U $USER"}, Interval: dur("5s"), Timeout: dur("1s")}
			}},
		{name: "healthcheck NONE", file: web(`healthcheck: {test: [NONE], interval: 5s}`)},
		{name: "healthcheck disabled", file: web(`healthcheck: {disable: true}`)},
		{name: "healthcheck disabled with NONE", file: web(`healthcheck: {disable: true, test: [NONE]}`)},
		{name: "healthcheck disable false", file: web(`healthcheck: {disable: false, test: [CMD, "true"]}`),
			edit: func(a *spec.App) { a.Health = &spec.Health{Exec: []string{"true"}} }},
		{name: "restart always", file: web("restart: always")},
		{name: "restart unless-stopped", file: web("restart: unless-stopped")},
		{name: "ignored top-level keys", file: "version: '3.8'\nname: shop\nx-common: {a: b}\n" + web("x-local: 1")},
		{name: "image from a variable", file: "services:\n  web:\n    image: nginx:${TAG}\n"},
		{name: "image default", file: "services:\n  web:\n    image: nginx:${UNSET:-1.27}\n"},
		{name: "empty variable keeps the - default unused", file: web("environment: {A: \"${EMPTY-unused}\", B: \"${EMPTY:-used}\"}"),
			edit: func(a *spec.App) { a.Env = map[string]string{"A": "", "B": "used"} }},
		{name: "interpolation forms", file: web("environment:\n  A: $$NAME\n  B: $NAME-x\n  C: ${NAME}x\n  D: ${UNSET-${NAME:-n}}\n  E: ${NAME:?needed}\n  F: ${EMPTY?}\n  G: cost $5 and $\n  H: '${UNSET:-}'"),
			edit: func(a *spec.App) {
				a.Env = map[string]string{"A": "$NAME", "B": "world-x", "C": "worldx", "D": "world", "E": "world", "F": "", "G": "cost $5 and $", "H": ""}
			}},
		{name: "anchors and merge keys", file: `x-base: &base
  image: nginx:1.27
  environment: &env {A: from-base, B: from-base}
x-more: &more
  environment: {A: from-more}
  command: [from-more]
services:
  web:
    <<: [*base, *more]
    command: [own]
`, edit: func(a *spec.App) {
			a.Env = map[string]string{"A": "from-base", "B": "from-base"}
			a.Command = []string{"own"}
		}},
		{name: "alias to a scalar and a list", file: "x-cmd: &cmd [sleep, '10']\nx-img: &img nginx:1.27\nservices:\n  web: {image: *img, command: *cmd}\n",
			edit: func(a *spec.App) { a.Command = []string{"sleep", "10"} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lookup := vars
			if c.vars != nil {
				lookup = c.vars
			}
			res, err := Convert([]byte(c.file), Options{Lookup: env(lookup)})
			if err != nil {
				t.Fatalf("Convert: %v\n%s", err, c.file)
			}
			if len(res.Apps) != 1 || res.Apps[0].Service != "web" {
				t.Fatalf("apps: %+v", res.Apps)
			}
			want := base()
			if c.edit != nil {
				c.edit(&want)
			}
			want.Normalize()
			if got := res.Apps[0].Spec; !reflect.DeepEqual(got, want) {
				t.Errorf("spec\n got %s\nwant %s", js(got), js(want))
			}
			var notes []string
			for _, n := range res.Notes {
				notes = append(notes, n.String())
			}
			if !reflect.DeepEqual(notes, c.notes) {
				t.Errorf("notes\n got %q\nwant %q", notes, c.notes)
			}
		})
	}
}

func TestConvertMemoryUnits(t *testing.T) {
	for in, want := range map[string]spec.ByteSize{
		"2b": 2, "1024kb": 1 << 20, "2048k": 2 << 20, "300m": 300 << 20, "300MB": 300 << 20, "1gb": 1 << 30, "1G": 1 << 30,
		"1.5g": 3 << 29, "1073741824": 1 << 30, "6291456": 6 << 20, "0": 0,
	} {
		c := &converter{lookup: env(nil)}
		got, ok := c.memory("m", &node{kind: scalarKind, tag: "!!str", text: in})
		if !ok || got != want {
			t.Errorf("%s: %d bytes, %v; want %d", in, got, c.problems, want)
		}
	}
	for _, bad := range []string{"512Mi", "1tb", "1.2.3g", "-1m", "1 g", "g", "1e3", "999999999999999999999g"} {
		c := &converter{lookup: env(nil)}
		if got, ok := c.memory("m", &node{kind: scalarKind, tag: "!!str", text: bad}); ok {
			t.Errorf("%s: %d bytes, want an error", bad, got)
		}
	}
	// A YAML integer is bytes.
	res, err := Convert([]byte(web("deploy: {resources: {limits: {memory: 67108864}}}")), Options{})
	if err != nil || res.Apps[0].Spec.Resources.Memory != 64<<20 {
		t.Fatalf("integer memory: %+v %v", res, err)
	}
}

func TestConvertSortsServicesAndPrefixes(t *testing.T) {
	file := "services:\n  web: {image: nginx}\n  api: {image: ghcr.io/x/api:2}\n  db: {image: postgres:16}\n"
	res, err := Convert([]byte(file), Options{Prefix: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range res.Apps {
		got = append(got, a.Service+"="+a.Spec.Name)
	}
	if want := []string{"api=shop-api", "db=shop-db", "web=shop-web"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("apps %v, want %v", got, want)
	}
	names, err := AppNames([]byte(file), "shop")
	if err != nil || !reflect.DeepEqual(names, []string{"shop-api", "shop-db", "shop-web"}) {
		t.Fatalf("AppNames: %v %v", names, err)
	}
}

// AppNames needs no variables and ignores what Convert would refuse.
func TestAppNamesReadsOnlyNames(t *testing.T) {
	file := "networks: {}\nservices:\n  web:\n    image: ${UNSET}\n    depends_on: [db]\n  db: {build: .}\n"
	names, err := AppNames([]byte(file), "")
	if err != nil || !reflect.DeepEqual(names, []string{"db", "web"}) {
		t.Fatalf("AppNames: %v %v", names, err)
	}
	for file, want := range map[string]string{
		"services: {}\n":                                     "services: define at least one service",
		"version: '3'\n":                                     "services: required",
		"services:\n  Web_1: {image: x}\n":                   `services.Web_1: the app name "Web_1" is not valid`,
		"services: [a]\n":                                    "services: want a mapping",
		"services:\n  a: {}\n  a: {}\n":                      `line 3: mapping key "a" already defined at line 2`,
		"services:\n  " + strings.Repeat("a", 33) + ": {}\n": "is not valid",
	} {
		if _, err := AppNames([]byte(file), ""); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", file, err, want)
		}
	}
	if _, err := AppNames([]byte("services:\n  web: {}\n"), strings.Repeat("p", 30)); err == nil || !strings.Contains(err.Error(), "services.web: the app name") {
		t.Errorf("long prefix: %v", err)
	}
}

// TestConvertRefuses checks the path and message of every refusal.
func TestConvertRefuses(t *testing.T) {
	vars := map[string]string{"EMPTY": "", "SET": "x"}
	cases := []struct {
		name, file, path, msg string
		dotenv                string
	}{
		// The top level.
		{name: "networks", file: web("") + "networks: {}\n", path: "networks", msg: "not supported: hivemind makes no networks: hivemind puts every container on Docker's default bridge network"},
		{name: "secrets", file: web("") + "secrets: {}\n", path: "secrets", msg: "not supported: hivemind has no secrets"},
		{name: "configs", file: web("") + "configs: {}\n", path: "configs", msg: "not supported: hivemind has no configs"},
		{name: "include", file: web("") + "include: [a.yaml]\n", path: "include", msg: "not supported: hivemind reads one file"},
		{name: "unknown top-level key", file: web("") + "service: {}\n", path: "service", msg: "not supported: hivemind reads services and volumes only"},
		{name: "no services", file: "version: '3'\n", path: "services", msg: "required: define at least one service"},
		{name: "null services", file: "services:\n", path: "services", msg: "required: define at least one service"},
		{name: "empty services", file: "services: {}\n", path: "services", msg: "define at least one service"},
		{name: "services list", file: "services: [web]\n", path: "services", msg: "want a mapping of service names to services"},
		{name: "too many services", file: manyServices(MaxServices + 1), path: "services", msg: "at most 100 services"},
		{name: "service not a mapping", file: "services:\n  web: nginx\n", path: "services.web", msg: "want a mapping"},
		{name: "invalid app name", file: "services:\n  My_Web: {image: nginx}\n", path: "services.My_Web", msg: `the app name "My_Web" is not valid: use 1 to 32 lowercase letters`},

		// Top-level volumes.
		{name: "external volume", file: web("") + "volumes: {data: {external: true}}\n", path: "volumes.data.external", msg: "not supported: hivemind creates each app's volumes"},
		{name: "volume driver", file: web("") + "volumes: {data: {driver: local}}\n", path: "volumes.data.driver", msg: "not supported: hivemind volumes use Docker's local driver"},
		{name: "volume driver_opts", file: web("") + "volumes: {data: {driver_opts: {}}}\n", path: "volumes.data.driver_opts", msg: "not supported"},
		{name: "volume labels", file: web("") + "volumes: {data: {labels: {}}}\n", path: "volumes.data.labels", msg: "not supported: hivemind labels each volume"},
		{name: "volume name", file: web("") + "volumes: {data: {name: other}}\n", path: "volumes.data.name", msg: "not supported: hivemind names each volume hivemind-<app>_<volume>"},
		{name: "volume other key", file: web("") + "volumes: {data: {size: 1}}\n", path: "volumes.data.size", msg: "not supported: a volume takes no settings"},
		{name: "volume scalar", file: web("") + "volumes: {data: 5}\n", path: "volumes.data", msg: "want {} or nothing"},
		{name: "volumes list", file: web("") + "volumes: [data]\n", path: "volumes", msg: "want a mapping of volume names"},

		// image, build and command.
		{name: "no image", file: "services:\n  web: {command: [x]}\n", path: "services.web.image", msg: "required"},
		{name: "null image", file: "services:\n  web: {image: null}\n", path: "services.web.image", msg: "required"},
		{name: "empty image", file: "services:\n  web: {image: ''}\n", path: "services.web.image", msg: "required"},
		{name: "image list", file: "services:\n  web: {image: [a]}\n", path: "services.web.image", msg: "want a string"},
		{name: "build", file: "services:\n  web: {build: .}\n", path: "services.web.build", msg: "not supported: build and push the image, then name it in image"},
		{name: "invalid image", file: "services:\n  web: {image: 'Nginx:'}\n", path: "services.web", msg: "image:"},
		{name: "empty command list", file: web("command: []"), path: "services.web.command", msg: "not supported: an empty command"},
		{name: "empty command string", file: web("command: ''"), path: "services.web.command", msg: "not supported: an empty command"},
		{name: "unbalanced single quote", file: web(`command: echo 'a`), path: "services.web.command", msg: "a single quote is not closed"},
		{name: "unbalanced double quote", file: web(`command: 'echo "a'`), path: "services.web.command", msg: "a double quote is not closed"},
		{name: "trailing backslash", file: web(`command: 'echo a\'`), path: "services.web.command", msg: "the command ends with a backslash"},
		{name: "command mapping", file: web("command: {a: b}"), path: "services.web.command", msg: "want a string or a list of strings"},
		{name: "command nested list", file: web("command: [[a]]"), path: "services.web.command[0]", msg: "want a string"},
		{name: "entrypoint", file: web("entrypoint: [sh]"), path: "services.web.entrypoint", msg: "not supported: the app spec has no entrypoint"},

		// environment.
		{name: "env null value", file: web("environment: {A: }"), path: "services.web.environment.A", msg: "give a value: in Compose a variable with no value comes from the shell; write A: ${A} for that"},
		{name: "env bare key", file: web("environment: [A]"), path: "services.web.environment[0]", msg: "give a value: in Compose a bare A comes from the shell; write A=${A} for that"},
		{name: "env no key", file: web("environment: ['=x']"), path: "services.web.environment[0]", msg: "want KEY=VALUE"},
		{name: "env list item mapping", file: web("environment: [{A: b}]"), path: "services.web.environment[0]", msg: "want KEY=VALUE"},
		{name: "env set twice", file: web("environment: [A=1, A=2]"), path: "services.web.environment[1]", msg: "A is set twice"},
		{name: "env nested value", file: web("environment: {A: [1]}"), path: "services.web.environment.A", msg: "want a string, number or boolean"},
		{name: "env scalar", file: web("environment: A=1"), path: "services.web.environment", msg: "want a mapping, or a list of KEY=VALUE"},
		{name: "env invalid name", file: web("environment: {1A: x}"), path: "services.web", msg: `env "1A": names are letters`},
		{name: "env_file", file: web("env_file: .env"), path: "services.web.env_file", msg: "not supported: list the variables under environment"},

		// ports.
		{name: "container port only", file: web(`ports: ["80"]`), path: "services.web.ports[0]", msg: `not supported: a port needs a host port: write HOST:CONTAINER, such as "8080:80"`},
		{name: "container port as a number", file: web(`ports: [80]`), path: "services.web.ports[0]", msg: "not supported: a port needs a host port"},
		{name: "address and container port", file: web(`ports: ["127.0.0.1::80"]`), path: "services.web.ports[0]", msg: "not supported: a port needs a host port"},
		{name: "port range", file: web(`ports: ["8080-8081:80-81"]`), path: "services.web.ports[0]", msg: "not supported: port ranges are not supported: list each port"},
		{name: "container range", file: web(`ports: ["3000-3005"]`), path: "services.web.ports[0]", msg: "not supported: a port needs a host port"},
		{name: "protocol", file: web(`ports: ["8080:80/sctp"]`), path: "services.web.ports[0]", msg: "the protocol must be tcp or udp"},
		{name: "port not a number", file: web(`ports: ["a:b"]`), path: "services.web.ports[0]", msg: "the host and container ports must be numbers"},
		{name: "port out of range", file: web(`ports: ["70000:80"]`), path: "services.web", msg: "ports[0]: host and container ports must be between 1 and 65535"},
		{name: "port bind not an address", file: web(`ports: ["example.com:8080:80"]`), path: "services.web", msg: `ports[0]: bind "example.com" is not an IP address`},
		{name: "ports scalar", file: web(`ports: "8080:80"`), path: "services.web.ports", msg: "want a list"},
		{name: "port list item", file: web(`ports: [[1]]`), path: "services.web.ports[0]", msg: "want a string such as"},
		{name: "long port without published", file: web("ports: [{target: 80}]"), path: "services.web.ports[0].published", msg: "required: hivemind publishes every port on a host port"},
		{name: "long port without target", file: web("ports: [{published: 80}]"), path: "services.web.ports[0].target", msg: "required"},
		{name: "long port range", file: web(`ports: [{target: 80, published: "8080-8081"}]`), path: "services.web.ports[0].published", msg: "not supported: port ranges"},
		{name: "long port ingress", file: web("ports: [{target: 80, published: 80, mode: ingress}]"), path: "services.web.ports[0].mode", msg: "not supported: ingress balances a port across a swarm"},
		{name: "long port mode", file: web("ports: [{target: 80, published: 80, mode: other}]"), path: "services.web.ports[0].mode", msg: "must be host"},
		{name: "long port name", file: web("ports: [{target: 80, published: 80, name: http}]"), path: "services.web.ports[0].name", msg: "not supported: the app spec has no port names"},
		{name: "long port app_protocol", file: web("ports: [{target: 80, published: 80, app_protocol: http}]"), path: "services.web.ports[0].app_protocol", msg: "not supported"},
		{name: "long port protocol", file: web("ports: [{target: 80, published: 80, protocol: sctp}]"), path: "services.web.ports[0].protocol", msg: "must be tcp or udp"},
		{name: "long port target", file: web("ports: [{target: http, published: 80}]"), path: "services.web.ports[0].target", msg: "want a whole number"},
		{name: "long port published", file: web("ports: [{target: 80, published: http}]"), path: "services.web.ports[0].published", msg: "want a port number"},
		{name: "long port unknown key", file: web("ports: [{target: 80, published: 80, other: 1}]"), path: "services.web.ports[0].other", msg: "not supported: a port has target, published, host_ip, protocol and mode only"},
		{name: "same host port twice", file: web(`ports: ["8080:80", "8080:81"]`), path: "services.web", msg: "ports[1]: host port 8080/tcp listed twice"},

		// Service volumes.
		{name: "read-only volume", file: vol("data:/data:ro"), path: "services.web.volumes[0]", msg: "not supported: read-only mounts are not supported"},
		{name: "access mode", file: vol("data:/data:rw,z"), path: "services.web.volumes[0]", msg: "not supported: the access mode must be rw"},
		{name: "relative bind mount", file: vol("./data:/data"), path: "services.web.volumes[0]", msg: "not supported: bind mounts are not supported"},
		{name: "absolute bind mount", file: vol("/srv/data:/data"), path: "services.web.volumes[0]", msg: "not supported: bind mounts"},
		{name: "home bind mount", file: vol("~/data:/data"), path: "services.web.volumes[0]", msg: "not supported: bind mounts"},
		{name: "anonymous volume", file: vol("/data"), path: "services.web.volumes[0]", msg: "not supported: anonymous volumes are not supported"},
		{name: "too many colons", file: vol("data:/a:rw:x"), path: "services.web.volumes[0]", msg: "want VOLUME:PATH or VOLUME:PATH:rw"},
		{name: "undeclared volume", file: web("volumes: ['other:/data']"), path: "services.web.volumes[0]", msg: `volume "other" is not declared under the top-level volumes`},
		{name: "relative path", file: vol("data:data"), path: "services.web", msg: `volumes[0]: path "data" must be an absolute, clean path other than /`},
		{name: "volume name not valid", file: "volumes: {my_data: }\n" + web("volumes: ['my_data:/data']"), path: "services.web", msg: `volumes[0]: name "my_data"`},
		{name: "volume and replicas", file: vol("data:/data") + "    deploy: {replicas: 2}\n", path: "services.web", msg: "volumes: an app with volumes can have only 1 replica"},
		{name: "long bind", file: volLong("type: bind\nsource: ./x\ntarget: /x"), path: "services.web.volumes[0].type", msg: "not supported: bind mounts"},
		{name: "long tmpfs", file: volLong("type: tmpfs\ntarget: /x"), path: "services.web.volumes[0].type", msg: "not supported: tmpfs mounts are not supported"},
		{name: "long npipe", file: volLong("type: npipe\nsource: data\ntarget: /x"), path: "services.web.volumes[0].type", msg: "not supported: hivemind mounts named volumes only"},
		{name: "long without type", file: volLong("source: data\ntarget: /x"), path: "services.web.volumes[0].type", msg: "required: write type: volume"},
		{name: "long without source", file: volLong("type: volume\ntarget: /x"), path: "services.web.volumes[0].source", msg: "required"},
		{name: "long without target", file: volLong("type: volume\nsource: data"), path: "services.web.volumes[0].target", msg: "required"},
		{name: "long read_only", file: volLong("type: volume\nsource: data\ntarget: /x\nread_only: true"), path: "services.web.volumes[0].read_only", msg: "not supported: read-only mounts"},
		{name: "long volume options", file: volLong("type: volume\nsource: data\ntarget: /x\nvolume: {nocopy: true}"), path: "services.web.volumes[0].volume", msg: "not supported: a volume mount takes no options"},
		{name: "long unknown key", file: volLong("type: volume\nsource: data\ntarget: /x\nother: 1"), path: "services.web.volumes[0].other", msg: "not supported: a volume mount has type, source, target and read_only only"},
		{name: "shared volume", file: "volumes: {data: }\nservices:\n  a: {image: x, volumes: ['data:/a']}\n  b: {image: x, volumes: ['data:/b']}\n",
			path: "services.b.volumes[0]", msg: `not supported: volume "data" is also used by service a: a hivemind volume belongs to one app, as hivemind-<app>_<volume>`},
		{name: "volumes_from", file: web("volumes_from: [db]"), path: "services.web.volumes_from", msg: "not supported: a hivemind volume belongs to one app"},

		// deploy.
		{name: "zero replicas", file: web("deploy: {replicas: 0}"), path: "services.web.deploy.replicas", msg: "not supported: an app has at least 1 replica"},
		{name: "negative replicas", file: web("deploy: {replicas: -1}"), path: "services.web.deploy.replicas", msg: "want a whole number"},
		{name: "too many replicas", file: web("deploy: {replicas: 101}"), path: "services.web", msg: "replicas 101: must be between 1 and 100"},
		{name: "global mode", file: web("deploy: {mode: global}"), path: "services.web.deploy.mode", msg: "not supported: global mode runs a replica on every node"},
		{name: "job mode", file: web("deploy: {mode: replicated-job}"), path: "services.web.deploy.mode", msg: "not supported: jobs are not supported"},
		{name: "placement", file: web("deploy: {placement: {}}"), path: "services.web.deploy.placement", msg: "not supported: hivemind places replicas"},
		{name: "update_config", file: web("deploy: {update_config: {}}"), path: "services.web.deploy.update_config", msg: "not supported: hivemind updates one replica at a time"},
		{name: "rollback_config", file: web("deploy: {rollback_config: {}}"), path: "services.web.deploy.rollback_config", msg: "not supported"},
		{name: "endpoint_mode", file: web("deploy: {endpoint_mode: vip}"), path: "services.web.deploy.endpoint_mode", msg: "not supported"},
		{name: "deploy labels", file: web("deploy: {labels: {}}"), path: "services.web.deploy.labels", msg: "not supported"},
		{name: "pids", file: web("deploy: {resources: {limits: {pids: 10}}}"), path: "services.web.deploy.resources.limits.pids", msg: "not supported: hivemind sets no PID limit"},
		{name: "devices", file: web("deploy: {resources: {reservations: {devices: []}}}"), path: "services.web.deploy.resources.reservations.devices", msg: "not supported"},
		{name: "reservation differs", file: web("deploy: {resources: {limits: {cpus: 1}, reservations: {cpus: 0.5}}}"), path: "services.web.deploy.resources.reservations.cpus", msg: "not supported: hivemind uses one figure for both the reservation and the limit"},
		{name: "reservation without a limit", file: web("deploy: {resources: {reservations: {memory: 64m}}}"), path: "services.web.deploy.resources.reservations.memory", msg: "not supported: hivemind uses one figure"},
		{name: "memory unit", file: web("deploy: {resources: {limits: {memory: 512Mi}}}"), path: "services.web.deploy.resources.limits.memory", msg: "want a byte value such as 512m or 1gb"},
		{name: "memory too small", file: web("deploy: {resources: {limits: {memory: 1m}}}"), path: "services.web", msg: "resources.memory: must be 0 (no limit) or at least 6Mi"},
		{name: "cpus", file: web("deploy: {resources: {limits: {cpus: lots}}}"), path: "services.web.deploy.resources.limits.cpus", msg: "want a number of CPUs"},
		{name: "cpus infinite", file: web("deploy: {resources: {limits: {cpus: .inf}}}"), path: "services.web.deploy.resources.limits.cpus", msg: "want a number of CPUs"},
		{name: "restart_policy condition", file: web("deploy: {restart_policy: {condition: on-failure}}"), path: "services.web.deploy.restart_policy.condition", msg: "not supported: hivemind restarts every replica that exits"},
		{name: "restart_policy delay", file: web("deploy: {restart_policy: {delay: 5s}}"), path: "services.web.deploy.restart_policy.delay", msg: "not supported: hivemind restarts a replica after a backoff"},
		{name: "resources unknown key", file: web("deploy: {resources: {other: {}}}"), path: "services.web.deploy.resources.other", msg: "not supported"},

		// healthcheck.
		{name: "start_interval", file: web("healthcheck: {test: [CMD, x], start_interval: 1s}"), path: "services.web.healthcheck.start_interval", msg: "not supported: hivemind checks at one interval"},
		{name: "no test", file: web("healthcheck: {interval: 5s}"), path: "services.web.healthcheck.test", msg: "required: hivemind does not run the image's HEALTHCHECK"},
		{name: "disable and test", file: web("healthcheck: {disable: true, test: [CMD, x]}"), path: "services.web.healthcheck", msg: "disable: true and a test: keep one"},
		{name: "empty test", file: web("healthcheck: {test: []}"), path: "services.web.healthcheck.test", msg: "the list is empty"},
		{name: "empty test string", file: web("healthcheck: {test: ''}"), path: "services.web.healthcheck.test", msg: "the test is empty"},
		{name: "CMD alone", file: web("healthcheck: {test: [CMD]}"), path: "services.web.healthcheck.test", msg: "CMD needs a command after it"},
		{name: "CMD-SHELL with two", file: web("healthcheck: {test: [CMD-SHELL, a, b]}"), path: "services.web.healthcheck.test", msg: "CMD-SHELL takes one string after it"},
		{name: "test kind", file: web("healthcheck: {test: [RUN, x]}"), path: "services.web.healthcheck.test[0]", msg: "must be CMD, CMD-SHELL or NONE"},
		{name: "NONE with more", file: web("healthcheck: {test: [NONE, x]}"), path: "services.web.healthcheck.test", msg: "NONE takes nothing after it"},
		{name: "duration without a unit", file: web("healthcheck: {test: [CMD, x], interval: 30}"), path: "services.web.healthcheck.interval", msg: "want a duration such as 30s"},
		{name: "negative duration", file: web("healthcheck: {test: [CMD, x], timeout: -1s}"), path: "services.web.healthcheck.timeout", msg: "want a duration"},
		{name: "retries", file: web("healthcheck: {test: [CMD, x], retries: many}"), path: "services.web.healthcheck.retries", msg: "want a whole number"},
		{name: "disable not a boolean", file: web("healthcheck: {disable: maybe}"), path: "services.web.healthcheck.disable", msg: "want true or false"},
		{name: "interval too short", file: web("healthcheck: {test: [CMD, x], interval: 10ms}"), path: "services.web", msg: "health.interval: must be between 1s and 1h"},
		{name: "healthcheck unknown key", file: web("healthcheck: {test: [CMD, x], other: 1}"), path: "services.web.healthcheck.other", msg: "not supported"},

		// restart.
		{name: "restart no", file: web("restart: no"), path: "services.web.restart", msg: "not supported: hivemind restarts every replica that exits"},
		{name: "restart on-failure", file: web("restart: on-failure"), path: "services.web.restart", msg: "not supported: hivemind restarts every replica"},
		{name: "restart on-failure:3", file: web("restart: on-failure:3"), path: "services.web.restart", msg: "not supported"},
		{name: "restart other", file: web("restart: sometimes"), path: "services.web.restart", msg: "must be always or unless-stopped"},

		// Other service keys.
		{name: "depends_on", file: web("depends_on: [db]"), path: "services.web.depends_on", msg: "not supported: hivemind starts each app on its own, with no name another app can find it by; one app reaches another at a published port"},
		{name: "service networks", file: web("networks: [front]"), path: "services.web.networks", msg: "not supported: hivemind makes no networks: hivemind puts every container on Docker's default bridge network, where containers on one node can reach each other's ports"},
		{name: "container_name", file: web("container_name: web"), path: "services.web.container_name", msg: "not supported: hivemind names each container hivemind-<app>-<replica>-g<generation>"},
		{name: "labels", file: web("labels: {a: b}"), path: "services.web.labels", msg: "not supported: hivemind labels each container"},
		{name: "links", file: web("links: [db]"), path: "services.web.links", msg: "not supported: hivemind starts each app on its own"},
		{name: "scale", file: web("scale: 2"), path: "services.web.scale", msg: "not supported: use deploy.replicas"},
		{name: "extends", file: web("extends: {service: base}"), path: "services.web.extends", msg: "not supported: hivemind reads one file"},
		{name: "profiles", file: web("profiles: [debug]"), path: "services.web.profiles", msg: "not supported: every service in the file becomes an app"},
		{name: "hostname", file: web("hostname: web"), path: "services.web.hostname", msg: "not supported"},
		{name: "user", file: web("user: '1000'"), path: "services.web.user", msg: "not supported"},
		{name: "mem_limit", file: web("mem_limit: 1g"), path: "services.web.mem_limit", msg: "not supported: use deploy.resources.limits.memory"},
		{name: "privileged", file: web("privileged: true"), path: "services.web.privileged", msg: "not supported: the app spec has no equivalent"},
		{name: "cap_add", file: web("cap_add: [NET_ADMIN]"), path: "services.web.cap_add", msg: "not supported"},
		{name: "null refused key", file: web("depends_on:"), path: "services.web.depends_on", msg: "not supported"},

		// Interpolation.
		{name: "unset variable", file: web("environment: {A: $UNSET}"), path: "services.web.environment.A", msg: "variable UNSET is not set: set it in the environment or in .env, or give a default with ${UNSET:-default}"},
		{name: "unset braced variable", file: "services:\n  web: {image: 'nginx:${UNSET}'}\n", path: "services.web.image", msg: "variable UNSET is not set"},
		{name: "required with a message", file: web("environment: {A: '${UNSET:?set UNSET to the database URL}'}"), path: "services.web.environment.A", msg: "variable UNSET: set UNSET to the database URL"},
		{name: "required empty", file: web("environment: {A: '${EMPTY:?}'}"), path: "services.web.environment.A", msg: "variable EMPTY is empty"},
		{name: "required unset", file: web("environment: {A: '${UNSET?}'}"), path: "services.web.environment.A", msg: "variable UNSET is not set"},
		{name: "replacement form", file: web("environment: {A: '${SET:+x}'}"), path: "services.web.environment.A", msg: "the forms are ${SET}, ${SET:-default}"},
		{name: "unclosed brace", file: web("environment: {A: '${SET'}"), path: "services.web.environment.A", msg: "a ${ has no closing }"},
		{name: "bad name", file: web("environment: {A: '${1}'}"), path: "services.web.environment.A", msg: "a ${...} must start with a variable name"},
		{name: "unset in a default", file: web("environment: {A: '${UNSET:-${ALSO_UNSET}}'}"), path: "services.web.environment.A", msg: "variable ALSO_UNSET is not set"},
		{name: "deep nesting", file: web("environment: {A: '" + strings.Repeat("${U:-", 20) + "x" + strings.Repeat("}", 20) + "'}"), path: "services.web.environment.A", msg: "variables nest more than 16 deep"},

		// .env.
		{name: "dotenv bad line", file: web(""), dotenv: "A=1\nnot a variable\n", path: ".env line 2", msg: "want VAR=VALUE"},
		{name: "dotenv open quote", file: web(""), dotenv: "A='x\n", path: ".env line 1", msg: "the single quote is not closed"},
		{name: "dotenv open double quote", file: web(""), dotenv: "A=\"x\n", path: ".env line 1", msg: "the double quote is not closed"},
		{name: "dotenv after quote", file: web(""), dotenv: "A='x' y\n", path: ".env line 1", msg: "text after the closing quote"},
		{name: "dotenv unset variable", file: web(""), dotenv: "A=${UNSET}\n", path: ".env line 1", msg: "variable UNSET is not set"},

		// YAML.
		{name: "duplicate key", file: "services:\n  web:\n    image: a\n    image: b\n", path: "", msg: `line 4: mapping key "image" already defined at line 3`},
		{name: "duplicate merge key", file: "x: &x {a: 1}\nservices:\n  web: {<<: *x, <<: *x, image: a}\n", path: "", msg: `mapping key "<<" already defined`},
		{name: "syntax error", file: "services:\n  web: [\n", path: "", msg: "yaml:"},
		{name: "two documents", file: web("") + "---\n" + web(""), path: "", msg: "the file holds more than one YAML document"},
		{name: "empty file", file: "", path: "", msg: "the file is empty"},
		{name: "comments only", file: "# nothing\n", path: "", msg: "the file is empty"},
		{name: "null document", file: "~\n", path: "", msg: "the file is empty"},
		{name: "top-level list", file: "- a\n", path: "", msg: "the top level of the file must be a mapping"},
		{name: "alias cycle", file: "a: &a [*a]\n", path: "", msg: "line 1: alias *a is inside the value it refers to"},
		{name: "unknown alias", file: "a: *b\n", path: "", msg: "unknown anchor 'b' referenced"},
		{name: "reset tag", file: web("command: !reset []"), path: "", msg: "the !reset tag is for merging Compose files, and hivemind reads one file"},
		{name: "override tag", file: web("environment: !override {}"), path: "", msg: "the !override tag is for merging Compose files"},
		{name: "binary tag", file: web("command: !!binary aGk="), path: "", msg: "the YAML tag !!binary is not supported"},
		{name: "custom tag", file: web("command: !mine x"), path: "", msg: "the YAML tag !mine is not supported"},
		{name: "merge of a list", file: "x: &x [1]\nservices:\n  web: {<<: *x, image: a}\n", path: "", msg: "a merge key (<<) takes a mapping or a list of mappings"},
		{name: "merge of a scalar", file: "services:\n  web: {<<: 5, image: a}\n", path: "", msg: "a merge key (<<) takes a mapping"},
		{name: "complex key", file: "? [a]\n: b\n", path: "", msg: "a mapping key must be a string"},
		{name: "bad int tag", file: web("environment: {A: !!int x}"), path: "", msg: "the value is not a valid !!int"},
		{name: "too large", file: web("") + "x-pad: '" + strings.Repeat("a", MaxFileBytes) + "'\n", path: "", msg: "the file is larger than 1048576 bytes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := Options{Lookup: env(vars)}
			if c.dotenv != "" {
				opts.DotEnv = []byte(c.dotenv)
			}
			res, err := Convert([]byte(c.file), opts)
			if err == nil {
				t.Fatalf("converted: %+v", res)
			}
			var ce *Error
			if !errors.As(err, &ce) {
				t.Fatalf("error %T %v is not a *compose.Error", err, err)
			}
			for _, p := range ce.Problems {
				if p.Path == c.path && strings.Contains(p.Msg, c.msg) {
					return
				}
			}
			t.Fatalf("no problem at %q containing %q in:\n%v", c.path, c.msg, err)
		})
	}
}

func manyServices(n int) string {
	var b strings.Builder
	b.WriteString("services:\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "  s%d: {image: nginx}\n", i)
	}
	return b.String()
}

func vol(mount string) string {
	return "volumes: {data: }\n" + web("volumes: ['"+mount+"']")
}

func volLong(body string) string {
	s := "volumes: {data: }\nservices:\n  web:\n    image: nginx\n    volumes:\n"
	for i, line := range strings.Split(body, "\n") {
		if i == 0 {
			s += "      - " + line + "\n"
		} else {
			s += "        " + line + "\n"
		}
	}
	return s
}

// TestConvertReportsEveryProblemSorted checks that one error holds every
// refusal, sorted by path with indexes compared as numbers.
func TestConvertReportsEveryProblemSorted(t *testing.T) {
	ports := make([]string, 11)
	for i := range ports {
		ports[i] = fmt.Sprintf(`"%d:80"`, 8000+i)
	}
	ports[2], ports[10] = `"80"`, `"81"`
	file := "networks: {}\nservices:\n  zz:\n    image: x\n    depends_on: [a]\n  aa:\n    build: .\n    ports: [" + strings.Join(ports, ", ") + "]\n    restart: no\n"
	_, err := Convert([]byte(file), Options{})
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v", err)
	}
	var paths []string
	for _, p := range ce.Problems {
		paths = append(paths, p.Path)
	}
	want := []string{"networks", "services.aa.build", "services.aa.ports[2]", "services.aa.ports[10]", "services.aa.restart", "services.zz.depends_on"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths %q, want %q", paths, want)
	}
	if !strings.HasPrefix(err.Error(), "6 problems:\n  networks: not supported:") {
		t.Fatalf("message:\n%s", err)
	}
	_, err = Convert([]byte(web("depends_on: [a]")), Options{})
	if err == nil || err.Error() != "services.web.depends_on: not supported: hivemind starts each app on its own, with no name another app can find it by; one app reaches another at a published port" {
		t.Fatalf("one problem: %v", err)
	}
}

// TestErrorsNeverHoldValues puts a marker in environment values and in
// variables, makes each conversion fail in a different way, and checks that
// no message holds the marker.
func TestErrorsNeverHoldValues(t *testing.T) {
	const secret = "s3cr3t-VALUE"
	vars := map[string]string{"SECRET": secret, "SECRET_QUOTE": secret + `"`}
	files := []string{
		web("environment: {PASSWORD: " + secret + ", BAD: }"),
		web("environment: {PASSWORD: !!int " + secret + "}"),
		web("environment: {PASSWORD: !!float '" + secret + "'}"),
		web("environment: {PASSWORD: !!bool " + secret + "}"),
		web("environment: ['PASSWORD=" + secret + "', 'PASSWORD=" + secret + "']"),
		web("environment: ['" + secret + "']"),
		web("environment: ['=" + secret + "']"),
		web("environment: [{PASSWORD: " + secret + "}]"),
		web("environment: {PASSWORD: \"" + secret + "\\0\"}"),
		web("environment: {PASSWORD: '${SECRET}', OTHER: '${SECRET:+x}'}"),
		web("environment: {PASSWORD: '${UNSET:-" + secret + "}${ALSO_UNSET}'}"),
		web("environment: {PASSWORD: '${SECRET'}"),
		web(`command: "echo ${SECRET_QUOTE}"`),
		web("command: 'echo \"" + secret + "'"),
		web(`ports: ["${SECRET}:80"]`),
		web(`ports: ["127.0.0.1:${SECRET}"]`),
		web("ports: [{target: '${SECRET}', published: '${SECRET}', protocol: '${SECRET}', mode: '${SECRET}'}]"),
		web("volumes: ['${SECRET}']") + "volumes: {data: }\n",
		web("volumes: ['data:/x:${SECRET}']") + "volumes: {data: }\n",
		web("deploy: {replicas: '${SECRET}', mode: '${SECRET}', resources: {limits: {cpus: '${SECRET}', memory: '${SECRET}'}}, restart_policy: {condition: '${SECRET}'}}"),
		web("healthcheck: {test: [CMD, '${SECRET}'], interval: '${SECRET}', timeout: '${SECRET}', retries: '${SECRET}', disable: '${SECRET}'}"),
		web("healthcheck: {test: ['${SECRET}', x]}"),
		web("restart: '${SECRET}'"),
	}
	for _, f := range files {
		for _, dotenv := range []string{"", "A='" + secret + "\n", "A=\"" + secret + "\" " + secret + "\n"} {
			opts := Options{Lookup: env(vars)}
			if dotenv != "" {
				opts.DotEnv = []byte(dotenv)
			}
			_, err := Convert([]byte(f), opts)
			if err == nil {
				t.Errorf("converted:\n%s", f)
				continue
			}
			if strings.Contains(err.Error(), "s3cr3t") {
				t.Errorf("the error holds a value:\n%v\nfile:\n%s", err, f)
			}
		}
	}
}

// TestBillionLaughs checks that alias expansion stops at maxAliasValues,
// quickly, and that a document that stays under it converts.
func TestBillionLaughs(t *testing.T) {
	var b strings.Builder
	b.WriteString("x-a: &a [lol, lol, lol, lol, lol, lol, lol, lol, lol, lol]\n")
	prev := "a"
	for _, name := range []string{"b", "c", "d", "e", "f", "g", "h", "i"} {
		fmt.Fprintf(&b, "x-%s: &%s [*%s, *%[3]s, *%[3]s, *%[3]s, *%[3]s, *%[3]s, *%[3]s, *%[3]s, *%[3]s, *%[3]s]\n", name, name, prev)
		prev = name
	}
	b.WriteString("services:\n  web: {image: nginx}\n")
	start := time.Now()
	_, err := Convert([]byte(b.String()), Options{})
	if err == nil || !strings.Contains(err.Error(), "YAML aliases expand to more than 100000 values") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("refusing took %v", d)
	}
	// 100 services that share one block are well under the limit.
	ok := "x-env: &env {A: '1', B: '2', C: '3'}\nservices:\n"
	for i := 0; i < MaxServices; i++ {
		ok += fmt.Sprintf("  s%d: {image: nginx, environment: *env}\n", i)
	}
	res, err := Convert([]byte(ok), Options{})
	if err != nil || len(res.Apps) != MaxServices {
		t.Fatalf("shared anchor: %v", err)
	}
}

func TestMergeKeyPrecedence(t *testing.T) {
	file := `x-a: &a {A: a, B: a}
x-b: &b {A: b, B: b, C: b}
services:
  web:
    image: nginx
    environment:
      B: own
      <<: [*a, *b]
`
	res, err := Convert([]byte(file), Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "a", "B": "own", "C": "b"}
	if got := res.Apps[0].Spec.Env; !reflect.DeepEqual(got, want) {
		t.Fatalf("env %v, want %v", got, want)
	}
}

// The environment wins over .env, and .env lines see the environment and
// the lines above them.
func TestDotEnvPrecedence(t *testing.T) {
	dotenv := "# comment\n\nexport TAG=1.25\nPORT=8080 # comment\nURL=\"http://h:${PORT}/\\tx\"\nRAW='$PORT \\'q\\''\nHASH=a#b\nLATER=${FROM_ENV}\nEMPTY=\nUNSET_HERE\n  SPACED = v  \n"
	file := "services:\n  web:\n    image: nginx:${TAG}\n    environment: {PORT: $PORT, URL: $URL, RAW: $RAW, HASH: $HASH, LATER: $LATER, EMPTY: $EMPTY, SPACED: $SPACED, D: '${UNSET_HERE:-unset}'}\n"
	res, err := Convert([]byte(file), Options{Lookup: env(map[string]string{"TAG": "1.27", "FROM_ENV": "env"}), DotEnv: []byte(dotenv)})
	if err != nil {
		t.Fatal(err)
	}
	a := res.Apps[0].Spec
	if a.Image != "nginx:1.27" {
		t.Errorf("image %q: the environment should win over .env", a.Image)
	}
	want := map[string]string{"PORT": "8080", "URL": "http://h:8080/\tx", "RAW": "$PORT 'q'", "HASH": "a#b", "LATER": "env", "EMPTY": "", "SPACED": "v", "D": "unset"}
	if !reflect.DeepEqual(a.Env, want) {
		t.Errorf("env\n got %q\nwant %q", a.Env, want)
	}
}

func TestSplitCommand(t *testing.T) {
	cases := map[string][]string{
		"a b  c":                 {"a", "b", "c"},
		"  a\tb\nc  ":            {"a", "b", "c"},
		`a 'b c' "d e"`:          {"a", "b c", "d e"},
		`'' ""`:                  {"", ""},
		`a'b'"c"d`:               {"abcd"},
		`"a\"b\\c\$d\e"`:         {`a"b\c$d\e`},
		`'a\b'`:                  {`a\b`},
		`a\ b \'c`:               {"a b", "'c"},
		"a\\\nb":                 {"ab"},
		"\"a\\\nb\"":             {"ab"},
		`sh -c 'echo $HOME; ls'`: {"sh", "-c", "echo $HOME; ls"},
		"":                       nil,
	}
	for in, want := range cases {
		got, err := splitCommand(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{`'a`, `"a`, `a\`, `"a\"`} {
		if _, err := splitCommand(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestInterpolate(t *testing.T) {
	look := env(map[string]string{"A": "x", "E": ""})
	cases := map[string]string{
		"plain": "plain", "$A": "x", "${A}": "x", "$A$A": "xx", "a$A-b": "ax-b", "$$A": "$A", "$$": "$", "$": "$",
		"$1": "$1", "$-": "$-", "${E:-d}": "d", "${E-d}": "", "${U-d}": "d", "${U:-}": "", "${A:-d}": "x",
		"${U:-${A}}": "x", "${U:-${U2:-z}}": "z", "${U:-a}}": "a}", "${A?}": "x", "${E?}": "", "${U:-$$}": "$",
	}
	for in, want := range cases {
		got, err := interpolate(in, look)
		if err != nil || got != want {
			t.Errorf("%q: %q, %v; want %q", in, got, err, want)
		}
	}
	for in, want := range map[string]string{
		"$U": "variable U is not set", "${U}": "variable U is not set", "${E:?}": "variable E is empty",
		"${U?msg}": "variable U: msg", "${A:+x}": "the forms are", "${A/x/y}": "the forms are", "${}": "must start with a variable name",
		"${A": "has no closing", "${U:-${A}": "has no closing",
	} {
		if _, err := interpolate(in, look); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", in, err, want)
		}
	}
}

func TestNaturalLess(t *testing.T) {
	in := []string{"b", "a[10]", "a[2]", "a[02x", "a", "a[1].z", "a[1]", "services.web", ".env line 10", ".env line 9"}
	sortByPath(in, func(s string) string { return s })
	want := []string{".env line 9", ".env line 10", "a", "a[1]", "a[1].z", "a[2]", "a[02x", "a[10]", "b", "services.web"}
	if !reflect.DeepEqual(in, want) {
		t.Fatalf("%q", in)
	}
}

func TestMarshalRoundTrips(t *testing.T) {
	res, err := Convert([]byte(web("ports: ['8080:80']\nenvironment: {PORT: '8080', B: 'true'}\nhealthcheck: {test: [CMD, x]}\ndeploy: {resources: {limits: {memory: 512m}}}")), Options{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := Marshal(res.Apps)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"name: web\n", "PORT: \"8080\"\n", "B: \"true\"\n", "memory: 512Mi\n", "bind: 127.0.0.1\n", "interval: 10s\n"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
}

// The top-level name is the project name, and may come from a variable. A
// file without one has none; a name Compose would refuse is refused.
// ProjectName makes one from a directory as Compose does.
func TestProjectName(t *testing.T) {
	lookup := func(k string) (string, bool) { return map[string]string{"P": "shop-2"}[k], k == "P" }
	for file, want := range map[string]string{
		"name: shop\nservices:\n  web: {image: nginx}\n":   "shop",
		"name: ${P}\nservices:\n  web: {image: nginx}\n":   "shop-2",
		"services:\n  web: {image: nginx}\n":               "",
		"name: null\nservices:\n  web: {image: nginx}\n":   "",
		"name: my_app\nservices:\n  web: {image: nginx}\n": "my_app",
	} {
		res, err := Convert([]byte(file), Options{Lookup: lookup})
		if err != nil {
			t.Errorf("%q: %v", file, err)
		} else if res.Name != want {
			t.Errorf("%q: name %q, want %q", file, res.Name, want)
		}
	}
	for _, file := range []string{"name: Shop\nservices:\n  web: {image: nginx}\n", "name: -x\nservices:\n  web: {image: nginx}\n",
		"name: [a]\nservices:\n  web: {image: nginx}\n"} {
		if _, err := Convert([]byte(file), Options{}); err == nil || !strings.Contains(err.Error(), "name: ") {
			t.Errorf("%q: %v, want the name refused", file, err)
		}
	}
	for dir, want := range map[string]string{"/srv/Shop.App": "shopapp", "/srv/my_app/": "my_app", "/srv/-x": "x", "/": "", "/srv/..": ""} {
		if got := ProjectName(dir); got != want {
			t.Errorf("ProjectName(%q) = %q, want %q", dir, got, want)
		}
	}
}

// A file that refers to one of hivemind's own variables, such as
// HIVEMIND_TOKEN, is refused, with or without a default and in any case,
// and the error names the variable, not its value. The lookup is never
// asked for one. A .env file may not set one, nor refer to one; it may set
// other variables.
func TestOwnVariablesRefused(t *testing.T) {
	const token = "hm_admin-token-value"
	var asked []string
	look := func(k string) (string, bool) {
		asked = append(asked, k)
		v, ok := map[string]string{"HIVEMIND_TOKEN": token, "HIVEMIND_SERVER": "https://h:4483", "hivemind_token": token, "OK": "fine"}[k]
		return v, ok
	}
	for _, tc := range []struct{ file, name string }{
		{web("environment: {T: '${HIVEMIND_TOKEN}'}"), "HIVEMIND_TOKEN"},
		{web("environment: {T: $HIVEMIND_TOKEN}"), "HIVEMIND_TOKEN"},
		{web("environment: ['T=${HIVEMIND_TOKEN:-none}']"), "HIVEMIND_TOKEN"},
		{web("environment: {T: '${HIVEMIND_TOKEN?}'}"), "HIVEMIND_TOKEN"},
		{web("environment: {T: '${hivemind_token}'}"), "hivemind_token"},
		{web("environment: {T: '${UNSET:-${HIVEMIND_SERVER}}'}"), "HIVEMIND_SERVER"},
		{web("command: 'echo $HIVEMIND_TOKEN'"), "HIVEMIND_TOKEN"},
		{"name: ${HIVEMIND_PROJECT:-shop}\n" + web(""), "HIVEMIND_PROJECT"},
		{"services:\n  web:\n    image: 'nginx:${HIVEMIND_TAG:-1}'\n", "HIVEMIND_TAG"},
	} {
		file, name := tc.file, tc.name
		asked = nil
		_, err := Convert([]byte(file), Options{Lookup: look})
		want := "variable " + name + ": a compose file cannot use the variables that start with HIVEMIND_"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", file, err, want)
			continue
		}
		if strings.Contains(err.Error(), token) {
			t.Errorf("%q: the error holds the token: %v", file, err)
		}
		for _, k := range asked {
			if strings.HasPrefix(strings.ToUpper(k), "HIVEMIND_") {
				t.Errorf("%q: the lookup was asked for %s", file, k)
			}
		}
	}

	// Names that only contain the word are other variables.
	res, err := Convert([]byte(web("environment: {A: '${MY_HIVEMIND_TOKEN:-a}', B: '${HIVEMINDX:-b}', C: $OK}")), Options{Lookup: look})
	if err != nil || res.Apps[0].Spec.Env["A"] != "a" || res.Apps[0].Spec.Env["B"] != "b" || res.Apps[0].Spec.Env["C"] != "fine" {
		t.Fatalf("other variables: %v %+v", err, res)
	}

	file := web("environment: {T: $T}")
	for _, tc := range []struct{ dotenv, want string }{
		{"HIVEMIND_SERVER=https://elsewhere\nT=x\n", ".env line 1: HIVEMIND_SERVER: .env cannot set the variables that start with HIVEMIND_"},
		{"export hivemind_token=x\nT=x\n", ".env line 1: hivemind_token: .env cannot set the variables"},
		{"T=${HIVEMIND_TOKEN}\n", ".env line 1: variable HIVEMIND_TOKEN: a compose file cannot use"},
		{"T=\"$HIVEMIND_TOKEN\"\n", ".env line 1: variable HIVEMIND_TOKEN: a compose file cannot use"},
	} {
		dotenv, want := tc.dotenv, tc.want
		_, err := Convert([]byte(file), Options{Lookup: look, DotEnv: []byte(dotenv)})
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), token) {
			t.Errorf(".env %q: %v, want %q", dotenv, err, want)
		}
	}
	res, err = Convert([]byte(file), Options{Lookup: look, DotEnv: []byte("T=from-dotenv\n")})
	if err != nil || res.Apps[0].Spec.Env["T"] != "from-dotenv" {
		t.Fatalf(".env with another variable: %v %+v", err, res)
	}
}
