package compose

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

type converter struct {
	prefix   string
	lookup   func(string) (string, bool)
	problems []Problem
	notes    []Note
	// declared holds the names under the top-level volumes.
	declared map[string]bool
	// volumeUser maps a volume to the first service that mounts it.
	volumeUser map[string]string
}

func (c *converter) add(path, format string, args ...any) {
	c.problems = append(c.problems, Problem{Path: path, Msg: fmt.Sprintf(format, args...)})
}

func (c *converter) refuse(path, reason string) { c.add(path, "not supported: %s", reason) }

func (c *converter) note(path, format string, args ...any) {
	c.notes = append(c.notes, Note{Path: path, Text: fmt.Sprintf(format, args...)})
}

func isExtension(key string) bool { return strings.HasPrefix(key, "x-") }

type service struct {
	name string
	n    *node
}

// file converts the whole file: the top-level keys, the volumes, and each
// service in name order.
func (c *converter) file(root *node) []App {
	for _, k := range root.keys {
		switch {
		case isExtension(k), k == "version", k == "name", k == "services", k == "volumes":
		default:
			c.refuse(k, reason(topReasons, k, "hivemind reads services and volumes only"))
		}
	}
	c.topVolumes(root.vals["volumes"])
	var apps []App
	for _, s := range c.services(root) {
		if a, ok := c.service(s.name, s.n); ok {
			apps = append(apps, a)
		}
	}
	return apps
}

// projectName reads the top-level name, the project name, which may come
// from a variable. It is "" when the file has none.
func (c *converter) projectName(root *node) string {
	n, ok := root.vals["name"]
	if !ok || n.kind == nullKind {
		return ""
	}
	s, ok := c.str("name", n)
	if !ok {
		return ""
	}
	if !projectRE.MatchString(s) || len(s) > maxProjectLen {
		c.add("name", "a project name is 1 to %d lowercase letters, digits, dashes and underscores, starting with a letter or digit", maxProjectLen)
		return ""
	}
	return s
}

// services returns the services, sorted by name.
func (c *converter) services(root *node) []service {
	n, ok := root.vals["services"]
	switch {
	case !ok || n.kind == nullKind:
		c.add("services", "required: define at least one service")
		return nil
	case n.kind != mapKind:
		c.add("services", "want a mapping of service names to services")
		return nil
	case len(n.keys) == 0:
		c.add("services", "define at least one service")
		return nil
	case len(n.keys) > MaxServices:
		c.add("services", "at most %d services", MaxServices)
		return nil
	}
	out := make([]service, 0, len(n.keys))
	for _, k := range n.keys {
		out = append(out, service{k, n.vals[k]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// appName returns the app name for a service, or records why it is not one.
func (c *converter) appName(svc string) (string, bool) {
	name := svc
	if c.prefix != "" {
		name = c.prefix + "-" + svc
	}
	if !spec.ValidName(name) {
		c.add("services."+svc, "the app name %q is not valid: use 1 to 32 lowercase letters, digits and dashes, starting and ending with a letter or digit", name)
		return "", false
	}
	return name, true
}

func (c *converter) topVolumes(n *node) {
	c.declared = map[string]bool{}
	if n == nil || n.kind == nullKind {
		return
	}
	if n.kind != mapKind {
		c.add("volumes", "want a mapping of volume names")
		return
	}
	for _, name := range n.keys {
		c.declared[name] = true
		path := "volumes." + name
		switch v := n.vals[name]; v.kind {
		case nullKind:
		case mapKind:
			for _, k := range v.keys {
				if !isExtension(k) {
					c.refuse(path+"."+k, reason(volumeReasons, k, "a volume takes no settings: write it as `"+name+": {}`"))
				}
			}
		default:
			c.add(path, "want {} or nothing")
		}
	}
}

func (c *converter) service(name string, n *node) (App, bool) {
	path := "services." + name
	before := len(c.problems)
	appName, _ := c.appName(name)
	if n.kind != mapKind {
		c.add(path, "want a mapping")
		return App{}, false
	}
	a := spec.App{Name: appName}
	for _, k := range n.keys {
		v, p := n.vals[k], path+"."+k
		switch k {
		case "image":
			a.Image = c.image(p, v)
		case "command":
			a.Command = c.command(p, v)
		case "environment":
			a.Env = c.environment(p, v)
		case "ports":
			a.Ports = c.ports(p, v)
		case "volumes":
			a.Volumes = c.volumes(name, p, v)
		case "deploy":
			c.deploy(p, v, &a)
		case "healthcheck":
			a.Health = c.healthcheck(p, v)
		case "restart":
			c.restart(p, v)
		default:
			if !isExtension(k) {
				c.refuse(p, reason(serviceReasons, k, "the app spec has no equivalent"))
			}
		}
	}
	if _, ok := n.vals["image"]; !ok {
		if _, build := n.vals["build"]; !build {
			c.add(path+".image", "required")
		}
	}
	if len(c.problems) > before {
		return App{}, false
	}
	a.Normalize()
	if err := a.Validate(); err != nil {
		for _, line := range strings.Split(err.Error(), "\n") {
			c.add(path, "%s", line)
		}
		return App{}, false
	}
	return App{Service: name, Spec: a}, true
}

// str reads a scalar as a string. Strings are interpolated; numbers and
// booleans are not, as in Compose.
func (c *converter) str(path string, v *node) (string, bool) {
	if v.kind != scalarKind {
		c.add(path, "want a string")
		return "", false
	}
	if v.tag != "!!str" {
		return v.text, true
	}
	s, err := interpolate(v.text, c.lookup)
	if err != nil {
		c.add(path, "%v", err)
		return "", false
	}
	return s, true
}

// integer reads a whole number of 0 or more, written as a number or as a
// string, which may come from a variable.
func (c *converter) integer(path string, v *node) (int, bool) {
	if v.kind == scalarKind && (v.tag == "!!int" || v.tag == "!!str") {
		s, ok := c.str(path, v)
		if !ok {
			return 0, false
		}
		if n, ok := wholeNumber(s); ok {
			return n, true
		}
	}
	c.add(path, "want a whole number, 0 or more")
	return 0, false
}

func wholeNumber(s string) (int, bool) {
	if s == "" || len(s) > 9 {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

func (c *converter) boolean(path string, v *node) (bool, bool) {
	if v.kind == scalarKind && (v.tag == "!!bool" || v.tag == "!!str") {
		s, ok := c.str(path, v)
		if !ok {
			return false, false
		}
		switch s {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	c.add(path, "want true or false")
	return false, false
}

func (c *converter) duration(path string, v *node) spec.Duration {
	if v.kind == scalarKind {
		s, ok := c.str(path, v)
		if !ok {
			return 0
		}
		if d, err := time.ParseDuration(s); err == nil && d >= 0 {
			return spec.Duration(d)
		}
	}
	c.add(path, "want a duration such as 30s or 1m30s")
	return 0
}

func (c *converter) image(path string, v *node) string {
	if v.kind == nullKind {
		c.add(path, "required")
		return ""
	}
	s, ok := c.str(path, v)
	if ok && s == "" {
		c.add(path, "required")
	}
	return s
}

const emptyCommand = "an empty command: leave command out to run the image's default command"

func (c *converter) command(path string, v *node) []string {
	switch v.kind {
	case nullKind:
		return nil
	case scalarKind:
		s, ok := c.str(path, v)
		if !ok {
			return nil
		}
		words, err := splitCommand(s)
		if err != nil {
			c.add(path, "%v", err)
			return nil
		}
		if len(words) == 0 {
			c.refuse(path, emptyCommand)
		}
		return words
	case seqKind:
		if len(v.items) == 0 {
			c.refuse(path, emptyCommand)
			return nil
		}
		out := make([]string, 0, len(v.items))
		for i, it := range v.items {
			if s, ok := c.str(fmt.Sprintf("%s[%d]", path, i), it); ok {
				out = append(out, s)
			}
		}
		return out
	}
	c.add(path, "want a string or a list of strings")
	return nil
}

// environment reads a mapping or a list of KEY=VALUE. Messages name keys,
// never values.
func (c *converter) environment(path string, v *node) map[string]string {
	env := map[string]string{}
	switch v.kind {
	case nullKind:
	case mapKind:
		for _, k := range v.keys {
			p, val := path+"."+k, v.vals[k]
			switch val.kind {
			case nullKind:
				c.add(p, "give a value: in Compose a variable with no value comes from the shell; write %s: ${%[1]s} for that", k)
			case scalarKind:
				if s, ok := c.str(p, val); ok {
					env[k] = s
				}
			default:
				c.add(p, "want a string, number or boolean")
			}
		}
	case seqKind:
		for i, it := range v.items {
			p := fmt.Sprintf("%s[%d]", path, i)
			if it.kind != scalarKind {
				c.add(p, "want KEY=VALUE")
				continue
			}
			s, ok := c.str(p, it)
			if !ok {
				continue
			}
			k, val, eq := strings.Cut(s, "=")
			switch {
			case !eq && validVarName(k):
				c.add(p, "give a value: in Compose a bare %s comes from the shell; write %[1]s=${%[1]s} for that", k)
			case !eq || k == "":
				c.add(p, "want KEY=VALUE")
			default:
				if _, dup := env[k]; dup {
					c.add(p, "%s is set twice", k)
					continue
				}
				env[k] = val
			}
		}
	default:
		c.add(path, "want a mapping, or a list of KEY=VALUE")
	}
	return env
}

const (
	needHostPort = `a port needs a host port: write HOST:CONTAINER, such as "8080:80"`
	portRange    = "port ranges are not supported: list each port"
)

func (c *converter) ports(path string, v *node) []spec.Port {
	if v.kind == nullKind {
		return nil
	}
	if v.kind != seqKind {
		c.add(path, "want a list")
		return nil
	}
	var out []spec.Port
	for i, it := range v.items {
		p := fmt.Sprintf("%s[%d]", path, i)
		before := len(c.problems)
		var port spec.Port
		switch it.kind {
		case scalarKind:
			port = c.shortPort(p, it)
		case mapKind:
			port = c.longPort(p, it)
		default:
			c.add(p, `want a string such as "8080:80", or a mapping`)
		}
		if len(c.problems) == before {
			out = append(out, port)
		}
	}
	return out
}

// shortPort reads [IP:]HOST:CONTAINER[/PROTOCOL]. An IPv6 address may be in
// brackets.
func (c *converter) shortPort(path string, v *node) spec.Port {
	var port spec.Port
	if v.tag != "!!str" {
		c.refuse(path, needHostPort)
		return port
	}
	s, ok := c.str(path, v)
	if !ok {
		return port
	}
	rest, proto, hasProto := strings.Cut(s, "/")
	if hasProto && proto != "tcp" && proto != "udp" {
		c.add(path, "the protocol must be tcp or udp")
		return port
	}
	i := strings.LastIndex(rest, ":")
	if i < 0 {
		c.refuse(path, needHostPort)
		return port
	}
	ctr, host, ip := rest[i+1:], rest[:i], ""
	if j := strings.LastIndex(host, ":"); j >= 0 {
		ip, host = host[:j], host[j+1:]
	}
	if len(ip) > 1 && ip[0] == '[' && ip[len(ip)-1] == ']' {
		ip = ip[1 : len(ip)-1]
	}
	if strings.Contains(host, "-") || strings.Contains(ctr, "-") {
		c.refuse(path, portRange)
		return port
	}
	if host == "" {
		c.refuse(path, needHostPort)
		return port
	}
	h, ok1 := wholeNumber(host)
	n, ok2 := wholeNumber(ctr)
	if !ok1 || !ok2 {
		c.add(path, "the host and container ports must be numbers")
		return port
	}
	port = spec.Port{Bind: ip, Host: h, Container: n, Protocol: proto}
	if ip == "" {
		port.Bind = spec.DefaultBind
		c.note(path, `published on %s:%d only, the hivemind default; Compose would publish it on every interface. To publish elsewhere, write "0.0.0.0:%d:%d" or another address`,
			spec.DefaultBind, h, h, n)
	}
	return port
}

func (c *converter) longPort(path string, v *node) spec.Port {
	var port spec.Port
	for _, k := range v.keys {
		val, p := v.vals[k], path+"."+k
		if val.kind == nullKind && longPortKeys[k] {
			continue
		}
		switch k {
		case "target":
			port.Container, _ = c.integer(p, val)
		case "published":
			if s, ok := c.str(p, val); ok {
				if strings.Contains(s, "-") {
					c.refuse(p, portRange)
				} else if n, ok := wholeNumber(s); ok {
					port.Host = n
				} else {
					c.add(p, "want a port number")
				}
			}
		case "host_ip":
			port.Bind, _ = c.str(p, val)
		case "protocol":
			if s, ok := c.str(p, val); ok {
				if s != "tcp" && s != "udp" {
					c.add(p, "must be tcp or udp")
				}
				port.Protocol = s
			}
		case "mode":
			if s, ok := c.str(p, val); ok {
				switch s {
				case "host":
				case "ingress":
					c.refuse(p, "ingress balances a port across a swarm; hivemind publishes it on the node that runs the replica, as mode: host does")
				default:
					c.add(p, "must be host")
				}
			}
		default:
			if !isExtension(k) {
				c.refuse(p, reason(portReasons, k, "a port has target, published, host_ip, protocol and mode only"))
			}
		}
	}
	if !present(v, "target") {
		c.add(path+".target", "required")
	}
	if !present(v, "published") {
		c.add(path+".published", "required: hivemind publishes every port on a host port")
	}
	if port.Bind == "" && port.Host > 0 {
		port.Bind = spec.DefaultBind
		c.note(path, "published on %s:%d only, the hivemind default; Compose would publish it on every interface. To publish elsewhere, set host_ip to 0.0.0.0 or another address",
			spec.DefaultBind, port.Host)
	}
	return port
}

var longPortKeys = map[string]bool{"target": true, "published": true, "host_ip": true, "protocol": true, "mode": true}

// present reports whether a mapping has key with a value other than null.
func present(m *node, key string) bool {
	v, ok := m.vals[key]
	return ok && v.kind != nullKind
}

const (
	bindMount = "bind mounts are not supported: hivemind mounts named volumes only; declare one under the top-level volumes and write name:/path"
	readOnly  = "read-only mounts are not supported: the app spec has none"
)

func (c *converter) volumes(svc, path string, v *node) []spec.Volume {
	if v.kind == nullKind {
		return nil
	}
	if v.kind != seqKind {
		c.add(path, "want a list")
		return nil
	}
	var out []spec.Volume
	for i, it := range v.items {
		p := fmt.Sprintf("%s[%d]", path, i)
		before := len(c.problems)
		var src, target string
		switch it.kind {
		case scalarKind:
			src, target = c.shortVolume(p, it)
		case mapKind:
			src, target = c.longVolume(p, it)
		default:
			c.add(p, `want a string such as "data:/data", or a mapping`)
		}
		if len(c.problems) > before {
			continue
		}
		if !c.declared[src] {
			c.add(p, "volume %q is not declared under the top-level volumes", src)
			continue
		}
		if other, used := c.volumeUser[src]; used && other != svc {
			c.refuse(p, fmt.Sprintf("volume %q is also used by service %s: a hivemind volume belongs to one app, as hivemind-<app>_<volume>", src, other))
			continue
		}
		c.volumeUser[src] = svc
		out = append(out, spec.Volume{Name: src, Path: target})
	}
	return out
}

// shortVolume reads VOLUME:PATH or VOLUME:PATH:rw.
func (c *converter) shortVolume(path string, v *node) (string, string) {
	s, ok := c.str(path, v)
	if !ok {
		return "", ""
	}
	parts := strings.Split(s, ":")
	switch {
	case len(parts) == 1:
		c.refuse(path, "anonymous volumes are not supported: name the volume, declare it under the top-level volumes, and write name:/path")
	case strings.HasPrefix(parts[0], ".") || strings.HasPrefix(parts[0], "/") || strings.HasPrefix(parts[0], "~"):
		c.refuse(path, bindMount)
	case len(parts) > 3 || parts[0] == "":
		c.add(path, "want VOLUME:PATH or VOLUME:PATH:rw")
	case len(parts) == 3 && parts[2] == "ro":
		c.refuse(path, readOnly)
	case len(parts) == 3 && parts[2] != "rw":
		c.refuse(path, "the access mode must be rw, or left out")
	default:
		return parts[0], parts[1]
	}
	return "", ""
}

func (c *converter) longVolume(path string, v *node) (string, string) {
	var typ, src, target string
	for _, k := range v.keys {
		val, p := v.vals[k], path+"."+k
		if val.kind == nullKind && longVolumeKeys[k] {
			continue
		}
		switch k {
		case "type":
			typ, _ = c.str(p, val)
		case "source":
			src, _ = c.str(p, val)
		case "target":
			target, _ = c.str(p, val)
		case "read_only":
			if ro, ok := c.boolean(p, val); ok && ro {
				c.refuse(p, readOnly)
			}
		default:
			if !isExtension(k) {
				c.refuse(p, reason(mountReasons, k, "a volume mount has type, source, target and read_only only"))
			}
		}
	}
	switch {
	case !present(v, "type"):
		c.add(path+".type", "required: write type: volume")
	case typ == "bind":
		c.refuse(path+".type", bindMount)
	case typ == "tmpfs":
		c.refuse(path+".type", "tmpfs mounts are not supported: hivemind mounts named volumes only")
	case typ != "volume":
		c.refuse(path+".type", "hivemind mounts named volumes only: write type: volume")
	}
	if !present(v, "source") {
		c.add(path+".source", "required: name a volume from the top-level volumes")
	}
	if !present(v, "target") {
		c.add(path+".target", "required")
	}
	return src, target
}

var longVolumeKeys = map[string]bool{"type": true, "source": true, "target": true, "read_only": true}

// deploy reads replicas, mode, resources and restart_policy, from
// https://github.com/compose-spec/compose-spec/blob/main/deploy.md.
func (c *converter) deploy(path string, v *node, a *spec.App) {
	if v.kind == nullKind {
		return
	}
	if v.kind != mapKind {
		c.add(path, "want a mapping")
		return
	}
	for _, k := range v.keys {
		val, p := v.vals[k], path+"."+k
		if val.kind == nullKind && (k == "replicas" || k == "mode" || k == "resources" || k == "restart_policy") {
			continue
		}
		switch k {
		case "replicas":
			if n, ok := c.integer(p, val); ok {
				if n < 1 {
					c.refuse(p, "an app has at least 1 replica; delete the app to stop it")
				} else {
					a.Replicas = n
				}
			}
		case "mode":
			if s, ok := c.str(p, val); ok {
				switch s {
				case "replicated":
				case "global":
					c.refuse(p, "global mode runs a replica on every node; set deploy.replicas instead")
				case "replicated-job", "global-job":
					c.refuse(p, "jobs are not supported: hivemind restarts a replica that exits")
				default:
					c.add(p, "must be replicated")
				}
			}
		case "resources":
			c.resources(p, val, a)
		case "restart_policy":
			c.restartPolicy(p, val)
		default:
			if !isExtension(k) {
				c.refuse(p, reason(deployReasons, k, "deploy has replicas, mode, resources and restart_policy only"))
			}
		}
	}
}

type resourceSet struct {
	cpu            float64
	memory         spec.ByteSize
	hasCPU, hasMem bool
}

func (c *converter) resources(path string, v *node, a *spec.App) {
	if v.kind != mapKind {
		c.add(path, "want a mapping")
		return
	}
	var limits, reservations resourceSet
	for _, k := range v.keys {
		val, p := v.vals[k], path+"."+k
		switch k {
		case "limits":
			limits = c.resourceSet(p, val)
		case "reservations":
			reservations = c.resourceSet(p, val)
		default:
			if !isExtension(k) {
				c.refuse(p, "resources has limits and reservations only")
			}
		}
	}
	const one = "hivemind uses one figure for both the reservation and the limit; leave reservations out, or make them equal to limits"
	if reservations.hasCPU && (!limits.hasCPU || reservations.cpu != limits.cpu) {
		c.refuse(path+".reservations.cpus", one)
	}
	if reservations.hasMem && (!limits.hasMem || reservations.memory != limits.memory) {
		c.refuse(path+".reservations.memory", one)
	}
	a.Resources = spec.Resources{CPU: limits.cpu, Memory: limits.memory}
}

func (c *converter) resourceSet(path string, v *node) resourceSet {
	var r resourceSet
	if v.kind == nullKind {
		return r
	}
	if v.kind != mapKind {
		c.add(path, "want a mapping")
		return r
	}
	for _, k := range v.keys {
		val, p := v.vals[k], path+"."+k
		switch k {
		case "cpus":
			if val.kind == nullKind {
				continue
			}
			r.cpu, r.hasCPU = c.cpus(p, val)
		case "memory":
			if val.kind == nullKind {
				continue
			}
			r.memory, r.hasMem = c.memory(p, val)
		case "pids":
			c.refuse(p, "hivemind sets no PID limit")
		case "devices", "generic_resources":
			c.refuse(p, "hivemind gives containers no devices")
		default:
			if !isExtension(k) {
				c.refuse(p, "hivemind reads cpus and memory only")
			}
		}
	}
	return r
}

func (c *converter) cpus(path string, v *node) (float64, bool) {
	if v.kind == scalarKind && v.tag != "!!bool" {
		if s, ok := c.str(path, v); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
			if err == nil && f >= 0 && !math.IsInf(f, 0) {
				return f, true
			}
		} else {
			return 0, false
		}
	}
	c.add(path, "want a number of CPUs, such as 0.5")
	return 0, false
}

// byteRE is a Compose byte value: an amount and b, k, kb, m, mb, g or gb.
var byteRE = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([kKmMgG]?)([bB]?)$`)

// memory reads a Compose byte value. k, m and g are powers of 1024, as
// Docker reads them, so 512m is 512Mi in the app spec. A bare number is bytes.
func (c *converter) memory(path string, v *node) (spec.ByteSize, bool) {
	if v.kind == scalarKind && (v.tag == "!!int" || v.tag == "!!str") {
		s, ok := c.str(path, v)
		if !ok {
			return 0, false
		}
		if m := byteRE.FindStringSubmatch(s); m != nil {
			f, err := strconv.ParseFloat(m[1], 64)
			mult := map[string]float64{"": 1, "k": 1 << 10, "m": 1 << 20, "g": 1 << 30}[strings.ToLower(m[2])]
			if err == nil && f*mult < 1<<62 {
				return spec.ByteSize(f * mult), true
			}
		}
	}
	c.add(path, "want a byte value such as 512m or 1gb: b, k, kb, m, mb, g or gb, where k is 1024")
	return 0, false
}

func (c *converter) restartPolicy(path string, v *node) {
	if v.kind != mapKind {
		c.add(path, "want a mapping")
		return
	}
	for _, k := range v.keys {
		val, p := v.vals[k], path+"."+k
		switch {
		case k == "condition":
			if val.kind == nullKind {
				continue
			}
			if s, ok := c.str(p, val); ok && s != "any" {
				if s == "none" || s == "on-failure" {
					c.refuse(p, "hivemind restarts every replica that exits, whatever its exit code; write any, or leave it out")
				} else {
					c.add(p, "must be any")
				}
			}
		case !isExtension(k):
			c.refuse(p, "hivemind restarts a replica after a backoff that doubles from 1 second to 5 minutes, with no limit on attempts")
		}
	}
}

func (c *converter) restart(path string, v *node) {
	if v.kind == nullKind {
		return
	}
	s, ok := c.str(path, v)
	if !ok {
		return
	}
	switch {
	case s == "always" || s == "unless-stopped":
	case s == "no" || s == "on-failure" || strings.HasPrefix(s, "on-failure:"):
		c.refuse(path, "hivemind restarts every replica that exits, whatever its exit code; write always or unless-stopped, or leave restart out")
	default:
		c.add(path, "must be always or unless-stopped")
	}
}

// healthcheck reads a check with an exec test. CMD-SHELL runs through
// /bin/sh -c, as Docker runs it.
func (c *converter) healthcheck(path string, v *node) *spec.Health {
	if v.kind == nullKind {
		return nil
	}
	if v.kind != mapKind {
		c.add(path, "want a mapping")
		return nil
	}
	h := &spec.Health{}
	var test *node
	disable := false
	for _, k := range v.keys {
		val, p := v.vals[k], path+"."+k
		if val.kind == nullKind && healthKeys[k] {
			continue
		}
		switch k {
		case "test":
			test = val
		case "interval":
			h.Interval = c.duration(p, val)
		case "timeout":
			h.Timeout = c.duration(p, val)
		case "start_period":
			h.StartPeriod = c.duration(p, val)
		case "retries":
			h.Retries, _ = c.integer(p, val)
		case "disable":
			disable, _ = c.boolean(p, val)
		case "start_interval":
			c.refuse(p, "hivemind checks at one interval, during the start period too")
		default:
			if !isExtension(k) {
				c.refuse(p, "a healthcheck has test, interval, timeout, retries, start_period and disable only")
			}
		}
	}
	if disable {
		if test != nil && !isNone(test) {
			c.add(path, "disable: true and a test: keep one")
		}
		return nil
	}
	if test == nil {
		c.add(path+".test", "required: hivemind does not run the image's HEALTHCHECK, so write the test here, or leave healthcheck out")
		return nil
	}
	exec, none := c.healthTest(path+".test", test)
	if none {
		return nil
	}
	h.Exec = exec
	return h
}

var healthKeys = map[string]bool{"test": true, "interval": true, "timeout": true, "start_period": true, "retries": true, "disable": true}

func isNone(test *node) bool {
	return test.kind == seqKind && len(test.items) > 0 && test.items[0].kind == scalarKind && test.items[0].text == "NONE"
}

// healthTest returns the command for a test, or none for ["NONE"].
func (c *converter) healthTest(path string, t *node) (exec []string, none bool) {
	switch t.kind {
	case scalarKind:
		s, ok := c.str(path, t)
		if ok && s == "" {
			c.add(path, "the test is empty")
		}
		return []string{"/bin/sh", "-c", s}, false
	case seqKind:
		if len(t.items) == 0 {
			c.add(path, "the list is empty: start it with CMD, CMD-SHELL or NONE")
			return nil, false
		}
		before := len(c.problems)
		words := make([]string, len(t.items))
		for i, it := range t.items {
			words[i], _ = c.str(fmt.Sprintf("%s[%d]", path, i), it)
		}
		if len(c.problems) > before {
			return nil, false
		}
		switch words[0] {
		case "NONE":
			if len(words) > 1 {
				c.add(path, "NONE takes nothing after it")
			}
			return nil, true
		case "CMD":
			if len(words) < 2 {
				c.add(path, "CMD needs a command after it")
			}
			return words[1:], false
		case "CMD-SHELL":
			if len(words) != 2 {
				c.add(path, "CMD-SHELL takes one string after it, the shell command")
				return nil, false
			}
			return []string{"/bin/sh", "-c", words[1]}, false
		}
		c.add(path+"[0]", "must be CMD, CMD-SHELL or NONE")
		return nil, false
	}
	c.add(path, "want a string or a list")
	return nil, false
}
