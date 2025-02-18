// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package app

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"

	"istio.io/api/annotation"
	meshconfig "istio.io/api/mesh/v1alpha1"
	"istio.io/istio/pilot/cmd/pilot-agent/config"
	"istio.io/istio/pilot/cmd/pilot-agent/options"
	"istio.io/istio/pilot/cmd/pilot-agent/status"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/util/network"
	"istio.io/istio/pkg/bootstrap"
	"istio.io/istio/pkg/cmd"
	"istio.io/istio/pkg/collateral"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/envoy"
	istio_agent "istio.io/istio/pkg/istio-agent"
	"istio.io/istio/pkg/log"
	"istio.io/istio/pkg/security"
	"istio.io/istio/pkg/slices"
	"istio.io/istio/pkg/util/protomarshal"
	"istio.io/istio/pkg/util/sets"
	"istio.io/istio/pkg/version"
	stsserver "istio.io/istio/security/pkg/stsservice/server"
	"istio.io/istio/security/pkg/stsservice/tokenmanager"
	cleaniptables "istio.io/istio/tools/istio-clean-iptables/pkg/cmd"
	iptables "istio.io/istio/tools/istio-iptables/pkg/cmd"
	iptableslog "istio.io/istio/tools/istio-iptables/pkg/log"
)

const (
	localHostIPv4 = "127.0.0.1"
	localHostIPv6 = "::1"
)

var (
	loggingOptions = log.DefaultOptions()
	proxyArgs      options.ProxyArgs
)

func NewRootCommand() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:          "pilot-agent",
		Short:        "Istio Pilot agent.",
		Long:         "Istio Pilot agent runs in the sidecar or gateway container and bootstraps Envoy.",
		SilenceUsage: true,
		FParseErrWhitelist: cobra.FParseErrWhitelist{
			// Allow unknown flags for backward-compatibility.
			UnknownFlags: true,
		},
	}

	// Attach the Istio logging options to the command.
	loggingOptions.AttachCobraFlags(rootCmd)

	cmd.AddFlags(rootCmd)

	proxyCmd := newProxyCommand()
	addFlags(proxyCmd)
	rootCmd.AddCommand(proxyCmd)
	rootCmd.AddCommand(requestCmd)
	rootCmd.AddCommand(waitCmd)
	rootCmd.AddCommand(version.CobraCommand())
	rootCmd.AddCommand(iptables.GetCommand())
	rootCmd.AddCommand(cleaniptables.GetCommand())

	rootCmd.AddCommand(collateral.CobraCommand(rootCmd, &doc.GenManHeader{
		Title:   "Istio Pilot Agent",
		Section: "pilot-agent CLI",
		Manual:  "Istio Pilot Agent",
	}))

	return rootCmd
}

func newProxyCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "proxy",
		Short: "XDS proxy agent",
		FParseErrWhitelist: cobra.FParseErrWhitelist{
			// Allow unknown flags for backward-compatibility.
			UnknownFlags: true,
		},
		PersistentPreRunE: configureLogging,
		RunE: func(c *cobra.Command, args []string) error {
			cmd.PrintFlags(c.Flags())
			log.Infof("Version %s", version.Info.String())

			logLimits()

			proxy, err := initProxy(args)
			if err != nil {
				return err
			}
			proxyConfig, err := config.ConstructProxyConfig(proxyArgs.MeshConfigFile, proxyArgs.ServiceCluster, options.ProxyConfigEnv, proxyArgs.Concurrency)
			if err != nil {
				return fmt.Errorf("failed to get proxy config: %v", err)
			}
			if out, err := protomarshal.ToYAML(proxyConfig); err != nil {
				log.Infof("Failed to serialize to YAML: %v", err)
			} else {
				log.Infof("Effective config: %s", out)
			}

			secOpts, err := options.NewSecurityOptions(proxyConfig, proxyArgs.StsPort, proxyArgs.TokenManagerPlugin)
			if err != nil {
				return err
			}

			// If security token service (STS) port is not zero, start STS server and
			// listen on STS port for STS requests. For STS, see
			// https://tools.ietf.org/html/draft-ietf-oauth-token-exchange-16.
			// STS is used for stackdriver or other Envoy services using google gRPC.
			if proxyArgs.StsPort > 0 {
				stsServer, err := initStsServer(proxy, secOpts.TokenManager)
				if err != nil {
					return err
				}
				defer stsServer.Stop()
			}

			// If we are using a custom template file (for control plane proxy, for example), configure this.
			if proxyArgs.TemplateFile != "" && proxyConfig.CustomConfigFile == "" {
				proxyConfig.ProxyBootstrapTemplatePath = proxyArgs.TemplateFile
			}

			envoyOptions := envoy.ProxyConfig{
				LogLevel:          proxyArgs.ProxyLogLevel,
				ComponentLogLevel: proxyArgs.ProxyComponentLogLevel,
				LogAsJSON:         loggingOptions.JSONEncoding,
				NodeIPs:           proxy.IPAddresses,
				Sidecar:           proxy.Type == model.SidecarProxy,
				OutlierLogPath:    proxyArgs.OutlierLogPath,
			}
			agentOptions := options.NewAgentOptions(proxy, proxyConfig)
			agent := istio_agent.NewAgent(proxyConfig, agentOptions, secOpts, envoyOptions)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			defer agent.Close()

			// If a status port was provided, start handling status probes.
			if proxyConfig.StatusPort > 0 {
				if err := initStatusServer(ctx, proxy, proxyConfig,
					agentOptions.EnvoyPrometheusPort, proxyArgs.EnableProfiling, agent, cancel); err != nil {
					return err
				}
			}

			go iptableslog.ReadNFLOGSocket(ctx)

			// On SIGINT or SIGTERM, cancel the context, triggering a graceful shutdown
			go cmd.WaitSignalFunc(cancel)

			// Start in process SDS, dns server, xds proxy, and Envoy.
			wait, err := agent.Run(ctx)
			if err != nil {
				return err
			}
			wait()
			return nil
		},
	}
}

func addFlags(proxyCmd *cobra.Command) {
	proxyArgs = options.NewProxyArgs()
	proxyCmd.PersistentFlags().StringVar(&proxyArgs.DNSDomain, "domain", "",
		"DNS domain suffix. If not provided uses ${POD_NAMESPACE}.svc.cluster.local")
	proxyCmd.PersistentFlags().StringVar(&proxyArgs.MeshConfigFile, "meshConfig", "./etc/istio/config/mesh",
		"File name for Istio mesh configuration. If not specified, a default mesh will be used. This may be overridden by "+
			"PROXY_CONFIG environment variable or proxy.istio.io/config annotation.")
	proxyCmd.PersistentFlags().IntVar(&proxyArgs.StsPort, "stsPort", 0,
		"HTTP Port on which to serve Security Token Service (STS). If zero, STS service will not be provided.")
	proxyCmd.PersistentFlags().StringVar(&proxyArgs.TokenManagerPlugin, "tokenManagerPlugin", tokenmanager.GoogleTokenExchange,
		"Token provider specific plugin name.")
	// DEPRECATED. Flags for proxy configuration
	proxyCmd.PersistentFlags().StringVar(&proxyArgs.ServiceCluster, "serviceCluster", constants.ServiceClusterName, "Service cluster")
	// Log levels are provided by the library https://github.com/gabime/spdlog, used by Envoy.
	proxyCmd.PersistentFlags().StringVar(&proxyArgs.ProxyLogLevel, "proxyLogLevel", "warning,misc:error",
		fmt.Sprintf("The log level used to start the Envoy proxy (choose from {%s, %s, %s, %s, %s, %s, %s})."+
			"Level may also include one or more scopes, such as 'info,misc:error,upstream:debug'",
			"trace", "debug", "info", "warning", "error", "critical", "off"))
	proxyCmd.PersistentFlags().IntVar(&proxyArgs.Concurrency, "concurrency", 0, "number of worker threads to run")
	// See https://www.envoyproxy.io/docs/envoy/latest/operations/cli#cmdoption-component-log-level
	proxyCmd.PersistentFlags().StringVar(&proxyArgs.ProxyComponentLogLevel, "proxyComponentLogLevel", "",
		"The component log level used to start the Envoy proxy. Deprecated, use proxyLogLevel instead")
	proxyCmd.PersistentFlags().StringVar(&proxyArgs.TemplateFile, "templateFile", "",
		"Go template bootstrap config")
	proxyCmd.PersistentFlags().StringVar(&proxyArgs.OutlierLogPath, "outlierLogPath", "",
		"The log path for outlier detection")
	proxyCmd.PersistentFlags().BoolVar(&proxyArgs.EnableProfiling, "profiling", true,
		"Enable profiling via web interface host:port/debug/pprof/.")
}

func initStatusServer(
	ctx context.Context,
	proxy *model.Proxy,
	proxyConfig *meshconfig.ProxyConfig,
	envoyPrometheusPort int,
	enableProfiling bool,
	agent *istio_agent.Agent,
	shutdown context.CancelFunc,
) error {
	o := options.NewStatusServerOptions(proxy, proxyConfig, agent)
	o.EnvoyPrometheusPort = envoyPrometheusPort
	o.EnableProfiling = enableProfiling
	o.Context = ctx
	o.Shutdown = shutdown
	statusServer, err := status.NewServer(*o)
	if err != nil {
		return err
	}
	go statusServer.Run(ctx)
	return nil
}

func initStsServer(proxy *model.Proxy, tokenManager security.TokenManager) (*stsserver.Server, error) {
	localHostAddr := localHostIPv4
	if proxy.IsIPv6() {
		localHostAddr = localHostIPv6
	} else {
		// if not ipv6-only, it can be ipv4-only or dual-stack
		// let InstanceIP decide the localhost
		netIP, _ := netip.ParseAddr(options.InstanceIPVar.Get())
		if netIP.Is6() && !netIP.IsLinkLocalUnicast() {
			localHostAddr = localHostIPv6
		}
	}
	stsServer, err := stsserver.NewServer(stsserver.Config{
		LocalHostAddr: localHostAddr,
		LocalPort:     proxyArgs.StsPort,
	}, tokenManager)
	if err != nil {
		return nil, err
	}
	return stsServer, nil
}

func getDNSDomain(podNamespace, domain string) string {
	if len(domain) == 0 {
		domain = podNamespace + ".svc." + constants.DefaultClusterLocalDomain
	}
	return domain
}

func configureLogging(_ *cobra.Command, _ []string) error {
	if err := log.Configure(loggingOptions); err != nil {
		return err
	}
	return nil
}

func initProxy(args []string) (*model.Proxy, error) {
	proxy := &model.Proxy{
		Type: model.SidecarProxy,
	}
	if len(args) > 0 {
		proxy.Type = model.NodeType(args[0])
		if !model.IsApplicationNodeType(proxy.Type) {
			return nil, fmt.Errorf("Invalid proxy Type: " + string(proxy.Type))
		}
	}

	podIP, _ := netip.ParseAddr(options.InstanceIPVar.Get()) // protobuf encoding of IP_ADDRESS type
	if podIP.IsValid() {
		// The first one must be the pod ip as we pick the first ip as pod ip in istiod.
		proxy.IPAddresses = []string{podIP.String()}
	}

	// Obtain all the IPs from the node
	proxyAddrs := make([]string, 0)
	if ipAddrs, ok := network.GetPrivateIPs(context.Background()); ok {
		proxyAddrs = append(proxyAddrs, ipAddrs...)
	}

	// No IP addresses provided, append 127.0.0.1 for ipv4 and ::1 for ipv6
	if len(proxyAddrs) == 0 {
		proxyAddrs = append(proxyAddrs, localHostIPv4, localHostIPv6)
	}

	// Get exclusions from traffic.sidecar.istio.io/excludeInterfaces
	excludeAddrs := getExcludeInterfaces()
	excludeAddrs.InsertAll(proxy.IPAddresses...) // prevent duplicate IPs
	proxyAddrs = slices.FilterInPlace(proxyAddrs, func(s string) bool {
		return !excludeAddrs.Contains(s)
	})

	proxy.IPAddresses = append(proxy.IPAddresses, proxyAddrs...)
	log.Debugf("proxy IPAddresses: %v", proxy.IPAddresses)

	// After IP addresses are set, let us discover IPMode.
	proxy.DiscoverIPMode()

	// Extract pod variables.
	proxy.ID = proxyArgs.PodName + "." + proxyArgs.PodNamespace

	// If not set, set a default based on platform - podNamespace.svc.cluster.local for
	// K8S
	proxy.DNSDomain = getDNSDomain(proxyArgs.PodNamespace, proxyArgs.DNSDomain)
	log.WithLabels("ips", proxy.IPAddresses, "type", proxy.Type, "id", proxy.ID, "domain", proxy.DNSDomain).Info("Proxy role")

	return proxy, nil
}

func getExcludeInterfaces() sets.String {
	excludeAddrs := sets.New[string]()

	// Get list of excluded interfaces from pod annotation
	// TODO: Discuss other input methods such as env, flag (ssuvasanth)
	annotations, err := bootstrap.ReadPodAnnotations("")
	if err != nil {
		log.Debugf("Reading podInfoAnnotations file to get excludeInterfaces was unsuccessful. Continuing without exclusions. msg: %v", err)
		return excludeAddrs
	}
	value, ok := annotations[annotation.SidecarTrafficExcludeInterfaces.Name]
	if !ok {
		log.Debugf("%s annotation is not present", annotation.SidecarTrafficExcludeInterfaces.Name)
		return excludeAddrs
	}
	exclusions := strings.Split(value, ",")

	// Find IP addr of excluded interfaces and add to a map for instant lookup
	for _, ifaceName := range exclusions {
		iface, err := net.InterfaceByName(ifaceName)
		if err != nil {
			log.Warnf("Unable to get interface %s: %v", ifaceName, err)
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			log.Warnf("Unable to get IP addr(s) of interface %s: %v", ifaceName, err)
			continue
		}

		for _, addr := range addrs {
			// Get IP only
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			default:
				continue
			}

			// handling ipv4 wrapping in ipv6
			ipAddr, okay := netip.AddrFromSlice(ip)
			if !okay {
				continue
			}
			unwrapAddr := ipAddr.Unmap()
			if !unwrapAddr.IsValid() || unwrapAddr.IsLoopback() || unwrapAddr.IsLinkLocalUnicast() || unwrapAddr.IsLinkLocalMulticast() || unwrapAddr.IsUnspecified() {
				continue
			}

			// Add to map
			excludeAddrs.Insert(unwrapAddr.String())
		}
	}

	log.Infof("Exclude IPs %v based on %s annotation", excludeAddrs, annotation.SidecarTrafficExcludeInterfaces.Name)
	return excludeAddrs
}

func logLimits() {
	out, err := exec.Command("bash", "-c", "ulimit -n").Output()
	outStr := strings.TrimSpace(string(out))
	if err != nil {
		log.Warnf("failed running ulimit command: %v", outStr)
	} else {
		log.Infof("Maximum file descriptors (ulimit -n): %v", outStr)
	}
}
