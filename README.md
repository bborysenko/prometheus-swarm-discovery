# Prometheus-Swarm service discovery

This is a POC that demonstrates Prometheus service discovery in Docker Swarm. At the moment, this POC only discovers Swarm services and their respective tasks, without attempting to discover nodes or other Swarm concepts.

## How it works

It is implemented as a dual tool that scrapes task/services/node information from the docker daemon and serves a [`static_config`](https://prometheus.io/docs/prometheus/latest/configuration/configuration/#static_config) json. The client fetches and writes the scrape targets to a file, that is then read by Prometheus. This uses the [`<file_sd_config>`](https://prometheus.io/docs/prometheus/latest/configuration/configuration/#<file_sd_config>) config
directive available in Prometheus. Eventually, the client may dissapear if prometheus supports fetching `static_config` from endpoints

```txt
Example deployment in a Swarm cluster
+------------------------+         +-----------------------+
|                        |         |                       |
| +--------------------+ |         | +-------------------+ |
| |                    | |         | |                   | |
| | Docker daemon      | |         | | Prometheus        | |
| |                    | |         | |                   | |
| +---------+----------+ |         | +---------+---------+ |
|           ^ docker.sock|         |           ^ shared volume
|           |            |         |           |           |
| +---------+----------+ |  tick   | +---------+---------+ |
| |                    <-------------+                   | |
| | Discovery server   | |         | | Discovery client  | |
| |                    +------------->                   | |
| +--------------------+ | *.json  | +-------------------+ |
|                        |         |                       |
+------------------------+         +-----------------------+
|                Manager |         |                Worker |
+------------------------+         +-----------------------+
```

The discovery server expects to have access to the Docker API. It's recommended that this access is done via `/var/run/docker.sock` without exposing the whole docker api endpoint outside the host. This implies that the server should be placed on the Swarm manager node.

The discovery client expects to have access to a shared volume with prometheus, as using the [`<file_sd_config>`](https://prometheus.io/docs/prometheus/latest/configuration/configuration/#<file_sd_config>) directive implies writing files to an prometheus filesystem

The discovery loop has several steps:
* the client initiates a discovery process at a configurable interval by issuing a request to the discovery server, indicating the prometheus service that will be used to scrape metrics
* The server reads the Docker Swarm API and collect all the services (and their tasks) + the networks they are connected to
* match the networks with the prometheus service ones, to find an ip/route accessible to it to scrape metrics
* serve a [`static_config`](https://prometheus.io/docs/prometheus/latest/configuration/configuration/#static_config) json to the client
* write the scrape targets in the configuration file (`<file_sd_config>`)

The rest is done by Prometheus. Whenever the scrape target configuration file is updated, Prometheus re-reads it and loads the current scrape targets.

## Running it
See example in the provided docker-compose.yaml file.

```
$ docker stack deploy -c docker-compose.yaml pro
```

## Annotating the services

To be able to find the scrape targets, service labels should be used
```
version: '3'

services:
  front-end:
    image: weaveworksdemos/front-end
    deploy:
      labels:
        prometheus.enable: "true" # Scrape this service
        prometheus.port: "9090" # optional (defaults to prometheus defaults := "80")
        prometheus.path: "/metrics" # optional (defaults to prometheus defaults := "/metrics")
        prometheus.job: "front-end" # prometheus job name. optional (defaults to service name in the stack eg: stack_front-end)
```

## Labels

The discovery tool attaches a set of labels to each target that are available during the [relabeling phase](https://prometheus.io/docs/operating/configuration/#<relabel_config>) of the service discovery in Prometheus:

* `__meta_swarm_stack_namespace`: The name of stack namespace
* `__meta_swarm_task_id`: The id of the Docker task. ex: `ww3kb7vkc81s1qt4sva0qo3cd`
* `__meta_swarm_task_name`: The name of the Docker task. ex: `stack_service.1.ww3kb7vkc81s1qt4sva0qo3cd`
* `__meta_swarm_task_desired_state`: The state of the task. You can filter them in relabeling (see example). ex: `running`
* `__meta_swarm_service_id`: The id of the Docker service. ex: `k95h1gs2kmc8espiom7s7jg3r`
* `__meta_swarm_service_name`: The name of the Docker service. ex: `stack_service`
* `__meta_swarm_node_id`: The id of node where the task is located. ex: `5pj6wnha7ncs7dcx9shtok48f`
* `__meta_swarm_node_hostname`: The hostname where the task is located. ex: `ip-172-31-8-170.ec2.internal`
* `__meta_swarm_label_<labelname>`: Labels for the service/tasks ex: `__meta_swarm_label_com_docker_stack_namespace=stack`
* `job`: As specified in the the label `prometheus.job`; the service name otherwise ex: `stack_service`

Labels starting with `__` are removed after the relabeling phase, so that these labels will not show up on time series directly.

## Configuration options

```sh
$ prometheus-swarm-discovery server --help
Starts Swarm service server

Usage:
  prometheus-swarm-discovery server [flags]

Flags:
  -h, --help              help for server
  -l, --loglevel string   Specify log level: debug, info, warn, error (default "info")
```

```sh
$ prometheus-swarm-discovery client --help
Starts Swarm service client

Usage:
  prometheus-swarm-discovery client [flags]

Flags:
  -h, --help                help for client
  -i, --interval int        The interval, in seconds, at which the discovery process is kicked off (default 30)
  -l, --loglevel string     Specify log level: debug, info, warn, error (default "info")
  -o, --output string       Output file that contains the Prometheus endpoints. (default "swarm-endpoints.json")
  -p, --prometheus string   Name of the Prometheus service (default "prometheus")
  -s, --server string       The prometheus-swarm-discovery server to ask for targets (default "http://prometheus-swarm-discovery:8080")
```
