package compose

// The reasons given for refusing a key. A key with no entry gets the
// fallback its caller passes. docs/compose.md lists them all.

const (
	noDiscovery = "hivemind starts each app on its own, and apps reach each other only through published ports"
	oneFile     = "hivemind reads one file and does not merge services; use YAML anchors and merge keys (<<) instead"
)

var topReasons = map[string]string{
	"networks": "hivemind has no networks between apps: apps reach each other only through published ports",
	"secrets":  "hivemind has no secrets: pass values in a service's environment, from variables",
	"configs":  "hivemind has no configs: build the files into the image",
	"include":  "hivemind reads one file: merge the files first, for example with docker compose config",
	"models":   "hivemind runs containers only",
}

var volumeReasons = map[string]string{
	"external":    "hivemind creates each app's volumes, named hivemind-<app>_<volume>, and cannot mount one made elsewhere",
	"name":        "hivemind names each volume hivemind-<app>_<volume>",
	"driver":      "hivemind volumes use Docker's local driver",
	"driver_opts": "hivemind volumes use Docker's local driver, with no options",
	"labels":      "hivemind labels each volume with hivemind.app and hivemind.volume, and nothing else",
}

var serviceReasons = map[string]string{
	"build":           "build and push the image, then name it in image",
	"depends_on":      noDiscovery,
	"links":           noDiscovery,
	"external_links":  noDiscovery,
	"networks":        "hivemind has no networks between apps: " + noDiscovery,
	"network_mode":    "hivemind gives each container Docker's default network: " + noDiscovery,
	"expose":          "apps reach each other only through published ports; list them under ports",
	"env_file":        "list the variables under environment; a value can come from .env beside the file, as ${VAR}",
	"container_name":  "hivemind names each container hivemind-<app>-<replica>-g<generation>",
	"hostname":        "hivemind does not set a container's hostname",
	"domainname":      "hivemind does not set a container's domain name",
	"labels":          "hivemind labels each container with hivemind.app, hivemind.replica, hivemind.generation and hivemind.routed, and nothing else",
	"entrypoint":      "the app spec has no entrypoint: put the whole command in command, or build an image with the entrypoint you need",
	"scale":           "use deploy.replicas",
	"extends":         oneFile,
	"profiles":        "every service in the file becomes an app; remove the services you do not want",
	"secrets":         "hivemind has no secrets: pass values in environment, from variables",
	"configs":         "hivemind has no configs: build the files into the image",
	"volumes_from":    "a hivemind volume belongs to one app",
	"tmpfs":           "hivemind mounts named volumes only",
	"cpus":            "use deploy.resources.limits.cpus",
	"mem_limit":       "use deploy.resources.limits.memory",
	"mem_reservation": "use deploy.resources.limits.memory: hivemind uses one figure for both the reservation and the limit",
	"user":            "the container runs as the image's user: set it in the image",
	"working_dir":     "the container starts in the image's working directory: set it in the image",
	"stop_signal":     "hivemind stops a container as docker stop does, with the image's stop signal",
	"logging":         "hivemind uses the engine's default log driver",
	"pull_policy":     "hivemind pulls an image when the node does not have it",
}

var portReasons = map[string]string{
	"name":         "the app spec has no port names",
	"app_protocol": "the app spec has no application protocols",
}

var mountReasons = map[string]string{
	"volume":      "a volume mount takes no options",
	"bind":        bindMount,
	"tmpfs":       "hivemind mounts named volumes only",
	"image":       "hivemind mounts named volumes only",
	"consistency": "a volume mount takes no options",
}

var deployReasons = map[string]string{
	"placement":       "hivemind places replicas by free CPU, memory and host ports; it has no constraints or preferences",
	"update_config":   "hivemind updates one replica at a time; see Rolling updates in docs/concepts.md",
	"rollback_config": "hivemind has no automatic rollback: deploy the old spec to roll back",
	"endpoint_mode":   noDiscovery,
	"labels":          "hivemind has no service labels",
}

func reason(table map[string]string, key, fallback string) string {
	if r, ok := table[key]; ok {
		return r
	}
	return fallback
}
