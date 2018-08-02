package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/util/strutil"
	"github.com/spf13/cobra"
)

const (
	enableLabel = "prometheus.enable"
	portLabel   = "prometheus.port"
	pathLabel   = "prometheus.path"
	jobLabel    = "prometheus.job"
)

type scrapeTask struct {
	Targets []string
	Labels  map[string]string
}

type scrapeTarget struct {
	Node    swarm.Node
	Service swarm.Service
	Task    swarm.Task
	IP      net.IP
}

type connectedTask struct {
	task swarm.Task
	ip   net.IP
}

// ServerOptions structure for all the cmd line flags
type ServerOptions struct {
	logLevel string
}

// ClientOptions structure for all the cmd line flags
type ClientOptions struct {
	logLevel          string
	serverURL         string
	prometheusService string
	output            string
	interval          int
}

var logger = logrus.New()
var discoveryServerOptions = ServerOptions{}
var discoveryClientOptions = ClientOptions{}

// finds a service by name
func findServiceByName(cli *client.Client, serviceName string) (swarm.Service, error) {

	var service swarm.Service

	serviceFilters := filters.NewArgs()
	serviceFilters.Add("name", serviceName)

	services, err := cli.ServiceList(context.Background(), types.ServiceListOptions{Filters: serviceFilters})
	if err != nil {
		return service, err
	}

	if len(services) == 0 {
		return service, fmt.Errorf("Could not find service %s", serviceName)
	}

	service = services[0]

	return service, nil
}

func findServicesByLabel(cli *client.Client, label string) ([]swarm.Service, error) {

	serviceFilters := filters.NewArgs()
	serviceFilters.Add("label", label)

	return cli.ServiceList(context.Background(), types.ServiceListOptions{Filters: serviceFilters})
}

func findServiceTasks(cli *client.Client, serviceID string) ([]swarm.Task, error) {

	taskFilters := filters.NewArgs()
	// should we remove filter these?
	// https://github.com/ContainerSolutions/prometheus-swarm-discovery/pull/1#issuecomment-292808012
	// taskFilters.Add("desired-state", string(swarm.TaskStateRunning))
	taskFilters.Add("service", serviceID)
	return cli.TaskList(context.Background(), types.TaskListOptions{Filters: taskFilters})
}

func findAllNodesMap(cli *client.Client) (map[string]swarm.Node, error) {
	nodeMap := make(map[string]swarm.Node)

	nodeFilters := filters.NewArgs()
	nodes, err := cli.NodeList(context.Background(), types.NodeListOptions{Filters: nodeFilters})
	if err != nil {
		return nil, err
	}

	for _, node := range nodes {
		nodeMap[node.ID] = node
	}

	return nodeMap, nil
}

func getNetworkIDsMap(service swarm.Service) map[string]bool {
	networkIDs := make(map[string]bool)

	for _, virtualIP := range service.Endpoint.VirtualIPs {
		networkIDs[virtualIP.NetworkID] = true
	}

	return networkIDs
}

func getScrapeTargets(cli *client.Client, prometheusServiceName string) ([]scrapeTarget, error) {

	prometheusService, err := findServiceByName(cli, prometheusServiceName)
	if err != nil {
		return nil, err
	}

	prometheusNetworkIDs := getNetworkIDsMap(prometheusService)

	prometheusEnabledServices, err := findServicesByLabel(cli, string(enableLabel)+"=true")
	if err != nil {
		return nil, err
	}

	scrapeTargets := make([]scrapeTarget, 0)

	nodes, err := findAllNodesMap(cli)
	if err != nil {
		return nil, err
	}

	for _, service := range prometheusEnabledServices {

		tasks, err := findServiceTasks(cli, service.ID)
		if err != nil {
			logger.Error(err)
			continue
		}

		connectedTasks := getConnectedTasks(tasks, prometheusNetworkIDs)

		for _, connectedTask := range connectedTasks {

			target := scrapeTarget{
				Node:    nodes[connectedTask.task.NodeID],
				Service: service,
				Task:    connectedTask.task,
				IP:      connectedTask.ip,
			}
			scrapeTargets = append(scrapeTargets, target)
		}
	}
	return scrapeTargets, nil

}

func buildLabels(target scrapeTarget) map[string]string {

	var joblabel = target.Service.Spec.Name
	if job, ok := target.Service.Spec.Labels[jobLabel]; ok {
		joblabel = job
	}

	labels := map[string]string{
		model.JobLabel: joblabel,

		// cAvdisor labels
		"container_label_com_docker_stack_namespace": string(target.Service.Spec.Labels["com.docker.stack.namespace"]),
		"container_label_com_docker_swarm_node_id": string(target.Node.ID),
		"container_label_com_docker_swarm_node_hostnmae": string(target.Node.Description.Hostname),
		"container_label_com_docker_swarm_service_id": string(target.Service.ID),
		"container_label_com_docker_swarm_service_name": string(target.Service.Spec.Name),
		"container_label_com_docker_swarm_task_id": string(target.Task.ID),
		"container_label_com_docker_swarm_task_name": fmt.Sprintf("%s.%d.%s", target.Service.Spec.Name, target.Task.Slot, target.Task.ID),

		// meta labels
		model.MetaLabelPrefix + "swarm_task_desired_state": string(target.Task.DesiredState),
	}

	// // We don't use this labels in
	//
	// if path, ok := target.Service.Spec.Labels[pathLabel]; ok {
	// 	labels[model.MetricsPathLabel] = path
	// }

	for k, v := range target.Service.Spec.Labels {
		labels[strutil.SanitizeLabelName(model.MetaLabelPrefix+"swarm_label_"+k)] = v
	}

	for k, v := range target.Task.Labels {
		labels[strutil.SanitizeLabelName(model.MetaLabelPrefix+"swarm_label_"+k)] = v
	}

	for k, v := range target.Task.Spec.ContainerSpec.Labels {
		labels[strutil.SanitizeLabelName(model.MetaLabelPrefix+"swarm_label_"+k)] = v
	}

	return labels
}

func buildTargets(target scrapeTarget) []string {
	var endpoint = target.IP.String()

	if port, ok := target.Service.Spec.Labels[portLabel]; ok {
		endpoint = endpoint + ":" + port
	}

	return []string{endpoint}
}

func buildScrapeTasks(scrapeTargets []scrapeTarget) []scrapeTask {
	tasks := make([]scrapeTask, 0)
	for _, target := range scrapeTargets {
		task := scrapeTask{
			Targets: buildTargets(target),
			Labels:  buildLabels(target),
		}
		tasks = append(tasks, task)
	}
	return tasks
}

func discoverSwarm(cli *client.Client, prometheusServiceName string) ([]scrapeTask, error) {

	scrapeTargetsMap, err := getScrapeTargets(cli, prometheusServiceName)
	if err != nil {
		return nil, err
	}

	return buildScrapeTasks(scrapeTargetsMap), nil
}

func getTaskIPs(task swarm.Task) map[string][]net.IP {
	ipsInNetwork := make(map[string][]net.IP)

	for _, netatt := range task.NetworksAttachments {

		if netatt.Network.Spec.Name == "ingress" || netatt.Network.DriverState.Name != "overlay" {
			continue
		}

		ips := make([]net.IP, 0)
		for _, ipcidr := range netatt.Addresses {
			ip, _, err := net.ParseCIDR(ipcidr)
			if err != nil {
				logger.Error(err)
				continue
			}
			ips = append(ips, ip)
		}

		if len(ips) > 0 {
			ipsInNetwork[netatt.Network.ID] = ips
		}
	}

	return ipsInNetwork

}

func getConnectedTasks(tasks []swarm.Task, networkIDs map[string]bool) []connectedTask {
	connectedTasks := make([]connectedTask, 0)

	for _, task := range tasks {
		ips := getTaskIPs(task)

		for taskNetworkID, taskIPs := range ips {
			if _, ok := networkIDs[taskNetworkID]; ok {
				connectedTasks = append(connectedTasks, connectedTask{
					task: task,
					ip:   taskIPs[0],
				})
			}
		}
	}

	return connectedTasks
}

func writeSDConfig(scrapeTasks []scrapeTask, output string) {
	jsonScrapeConfig, err := json.MarshalIndent(scrapeTasks, "", "  ")
	if err != nil {
		panic(err)
	}

	logger.Info("Writing Prometheus config file")

	err = ioutil.WriteFile(output, jsonScrapeConfig, 0644)
	if err != nil {
		panic(err)
	}
}

func discoveryServer(cmd *cobra.Command, args []string) {

	level, err := logrus.ParseLevel(discoveryServerOptions.logLevel)
	if err != nil {
		logger.Fatal("error when setting log level: ", err)
		return
	}

	logrus.SetLevel(level)

	cli, err := client.NewEnvClient()
	if err != nil {
		logger.Fatal("error when creating docker client: ", err)
		return
	}

	if _, err := cli.Ping(context.TODO()); err != nil {
		logger.Fatal("error when using docker client: ", err)
		return
	}

	r := gin.Default()

	r.GET("/targets/:prometheusService", func(c *gin.Context) {

		prometheusService := c.Param("prometheusService")
		scrapeTasks, err := discoverSwarm(cli, prometheusService)

		if err != nil {
			logger.Error(err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, scrapeTasks)

	})

	r.GET("/debug/:prometheusService", func(c *gin.Context) {

		prometheusService := c.Param("prometheusService")
		scrapeTasks, err := getScrapeTargets(cli, prometheusService)

		if err != nil {
			logger.Error(err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, scrapeTasks)

	})

	r.GET("/_health", func(c *gin.Context) {

		result, err := cli.Ping(context.TODO())

		if err != nil {
			logger.Error(err)
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}

		c.JSON(http.StatusOK, result)

	})

	r.Run() // listen and serve on 0.0.0.0:8080
}

func getServerTargets(serverClient *http.Client, serverURL string, prometheusService string, target interface{}) error {

	var url = serverURL + "/targets/" + prometheusService
	logger.Infof("Getting targets from %s", url)

	resp, err := serverClient.Get(url)
	if err != nil {
		logger.Error(err)
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(target)
}

func discoveryClient(cmd *cobra.Command, args []string) {
	var serverClient = &http.Client{Timeout: 10 * time.Second}

	level, err := logrus.ParseLevel(discoveryClientOptions.logLevel)
	if err != nil {
		logger.Fatal(err)
	}
	logger.Level = level

	for {
		targets := make([]scrapeTask, 0)
		err := getServerTargets(serverClient, discoveryClientOptions.serverURL, discoveryClientOptions.prometheusService, &targets)
		if err != nil {
			logger.Error(err)
			return
		}

		writeSDConfig(targets, discoveryClientOptions.output)
		time.Sleep(time.Duration(discoveryClientOptions.interval) * time.Second)
	}
}

func main() {

	var rootCmd = &cobra.Command{Use: "prometheus-swarm-discovery"}

	var cmdServer = &cobra.Command{
		Use:   "server",
		Short: "Starts Swarm service server",
		Run:   discoveryServer,
	}
	cmdServer.Flags().StringVarP(&discoveryServerOptions.logLevel, "loglevel", "l", "info", "Specify log level: debug, info, warn, error")
	rootCmd.AddCommand(cmdServer)

	var cmdClient = &cobra.Command{
		Use:   "client",
		Short: "Starts Swarm service client",
		Run:   discoveryClient,
	}
	cmdClient.Flags().StringVarP(&discoveryClientOptions.logLevel, "loglevel", "l", "info", "Specify log level: debug, info, warn, error")
	cmdClient.Flags().StringVarP(&discoveryClientOptions.serverURL, "server", "s", "http://prometheus-swarm-discovery:8080", "The prometheus-swarm-discovery server to ask for targets")
	cmdClient.Flags().StringVarP(&discoveryClientOptions.prometheusService, "prometheus", "p", "prometheus", "Name of the Prometheus service")
	cmdClient.Flags().StringVarP(&discoveryClientOptions.output, "output", "o", "swarm-endpoints.json", "Output file that contains the Prometheus endpoints.")
	cmdClient.Flags().IntVarP(&discoveryClientOptions.interval, "interval", "i", 30, "The interval, in seconds, at which the discovery process is kicked off")
	rootCmd.AddCommand(cmdClient)

	rootCmd.Execute()
}
