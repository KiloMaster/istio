// Copyright 2018 Istio Authors
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

// DEPRECATED - These commands are deprecated and will be removed in future releases.

package cmd

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ghodss/yaml"
	"github.com/hashicorp/go-multierror"
	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubeSchema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"istio.io/api/networking/v1alpha3"
	"istio.io/pkg/log"

	"istio.io/istio/galley/pkg/config/schema/collection"
	"istio.io/istio/galley/pkg/config/schema/collections"
	"istio.io/istio/istioctl/pkg/util/handlers"
	"istio.io/istio/pilot/pkg/config/kube/crd"
	"istio.io/istio/pilot/pkg/config/kube/crd/controller"
	"istio.io/istio/pilot/pkg/model"
	kubecfg "istio.io/istio/pkg/kube"
)

const (
	// Headings for short format listing of unknown types
	unknownShortOutputHeading = "NAME\tKIND\tNAMESPACE\tAGE"
)

var (
	istioContext     string
	istioAPIServer   string
	getAllNamespaces bool

	// Create a model.ConfigStore (or sortedConfigStore)
	clientFactory = newClient

	// sortWeight defines the output order for "get all".  We show the V3 types first.
	sortWeight = map[string]int{
		collections.IstioNetworkingV1Alpha3Gateways.Resource().Kind():         10,
		collections.IstioNetworkingV1Alpha3Virtualservices.Resource().Kind():  5,
		collections.IstioNetworkingV1Alpha3Destinationrules.Resource().Kind(): 3,
		collections.IstioNetworkingV1Alpha3Serviceentries.Resource().Kind():   1,
	}

	// mustList tracks which Istio types we SHOULD NOT silently ignore if we can't list.
	// The user wants reasonable error messages when doing `get all` against a different
	// server version.
	mustList = map[string]bool{
		collections.IstioNetworkingV1Alpha3Gateways.Resource().Kind():           true,
		collections.IstioNetworkingV1Alpha3Virtualservices.Resource().Kind():    true,
		collections.IstioNetworkingV1Alpha3Destinationrules.Resource().Kind():   true,
		collections.IstioNetworkingV1Alpha3Serviceentries.Resource().Kind():     true,
		collections.IstioConfigV1Alpha2Httpapispecs.Resource().Kind():           true,
		collections.IstioConfigV1Alpha2Httpapispecbindings.Resource().Kind():    true,
		collections.IstioMixerV1ConfigClientQuotaspecs.Resource().Kind():        true,
		collections.IstioMixerV1ConfigClientQuotaspecbindings.Resource().Kind(): true,
		collections.IstioAuthenticationV1Alpha1Policies.Resource().Kind():       true,
		collections.IstioRbacV1Alpha1Serviceroles.Resource().Kind():             true,
		collections.IstioRbacV1Alpha1Servicerolebindings.Resource().Kind():      true,
		collections.IstioRbacV1Alpha1Rbacconfigs.Resource().Kind():              true,
	}

	gatewayKind         = collections.IstioNetworkingV1Alpha3Gateways.Resource().Kind()
	virtualServiceKind  = collections.IstioNetworkingV1Alpha3Virtualservices.Resource().Kind()
	destinationRuleKind = collections.IstioNetworkingV1Alpha3Destinationrules.Resource().Kind()
	serviceEntryKind    = collections.IstioNetworkingV1Alpha3Serviceentries.Resource().Kind()

	// Headings for short format listing specific to type
	shortOutputHeadings = map[string]string{
		gatewayKind:         "GATEWAY NAME\tHOSTS\tNAMESPACE\tAGE",
		virtualServiceKind:  "VIRTUAL-SERVICE NAME\tGATEWAYS\tHOSTS\t#HTTP\t#TCP\tNAMESPACE\tAGE",
		destinationRuleKind: "DESTINATION-RULE NAME\tHOST\tSUBSETS\tNAMESPACE\tAGE",
		serviceEntryKind:    "SERVICE-ENTRY NAME\tHOSTS\tPORTS\tNAMESPACE\tAGE",
	}

	// Formatters for short format listing specific to type
	shortOutputters = map[string]func(model.Config, io.Writer){
		gatewayKind:         printShortGateway,
		virtualServiceKind:  printShortVirtualService,
		destinationRuleKind: printShortDestinationRule,
		serviceEntryKind:    printShortServiceEntry,
	}

	// all resources will be migrated out of config.istio.io to their own api group mapping to package path.
	// TODO(xiaolanz) legacy group exists until we find out a client for mixer
	legacyIstioAPIGroupVersion = kubeSchema.GroupVersion{
		Group:   "config.istio.io",
		Version: "v1alpha2",
	}

	postCmd = &cobra.Command{
		Use:        "create",
		Deprecated: "Use `kubectl create` instead (see https://kubernetes.io/docs/tasks/tools/install-kubectl)",
		Short:      "Create policies and rules",
		Example:    "istioctl create -f example-routing.yaml",
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) != 0 {
				c.Println(c.UsageString())
				return fmt.Errorf("create takes no arguments")
			}
			varr, others, err := readInputs()
			if err != nil {
				return err
			}
			if len(varr) == 0 && len(others) == 0 {
				return errors.New("nothing to create")
			}
			for _, config := range varr {
				if config.Namespace, err = handlers.HandleNamespaces(config.Namespace, namespace, defaultNamespace); err != nil {
					return err
				}

				var configClient model.ConfigStore
				if configClient, err = clientFactory(); err != nil {
					return err
				}
				var rev string
				if rev, err = configClient.Create(config); err != nil {
					return err
				}
				c.Printf("Created config %v at revision %v\n", config.Key(), rev)
			}

			if len(others) > 0 {
				if err = preprocMixerConfig(others); err != nil {
					return err
				}
				otherClient, resources, oerr := prepareClientForOthers(others)
				if oerr != nil {
					return oerr
				}
				var errs *multierror.Error
				var updated crd.IstioKind
				for _, config := range others {
					resource, ok := resources[config.Kind]
					if !ok {
						errs = multierror.Append(errs, fmt.Errorf("kind %s is not known", config.Kind))
						continue
					}
					err = otherClient.Post().
						Namespace(config.Namespace).
						Resource(resource.Name).
						Body(&config).
						Do().
						Into(&updated)
					if err != nil {
						errs = multierror.Append(errs, err)
						continue
					}
					key := model.Key(config.Kind, config.Name, config.Namespace)
					fmt.Printf("Created config %s at revision %v\n", key, updated.ResourceVersion)
				}
				if errs != nil {
					return errs
				}
			}

			return nil
		},
	}

	putCmd = &cobra.Command{
		Use:        "replace",
		Deprecated: "Use `kubectl apply` instead (see https://kubernetes.io/docs/tasks/tools/install-kubectl)",
		Short:      "Replace existing policies and rules",
		Example:    "istioctl replace -f example-routing.yaml",
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) != 0 {
				c.Println(c.UsageString())
				return fmt.Errorf("replace takes no arguments")
			}
			varr, others, err := readInputs()
			if err != nil {
				return err
			}
			if len(varr) == 0 && len(others) == 0 {
				return errors.New("nothing to replace")
			}
			for _, config := range varr {
				if config.Namespace, err = handlers.HandleNamespaces(config.Namespace, namespace, defaultNamespace); err != nil {
					return err
				}

				var configClient model.ConfigStore
				if configClient, err = clientFactory(); err != nil {
					return err
				}
				// fill up revision
				if config.ResourceVersion == "" {
					current := configClient.Get(config.GroupVersionKind(), config.Name, config.Namespace)
					if current != nil {
						config.ResourceVersion = current.ResourceVersion
					}
				}

				var newRev string
				if newRev, err = configClient.Update(config); err != nil {
					return err
				}

				fmt.Printf("Updated config %v to revision %v\n", config.Key(), newRev)
			}

			if len(others) > 0 {
				if err = preprocMixerConfig(others); err != nil {
					return err
				}
				otherClient, resources, oerr := prepareClientForOthers(others)
				if oerr != nil {
					return oerr
				}
				var errs *multierror.Error
				var current crd.IstioKind
				var updated crd.IstioKind
				for _, config := range others {
					resource, ok := resources[config.Kind]
					if !ok {
						errs = multierror.Append(errs, fmt.Errorf("kind %s is not known", config.Kind))
						continue
					}
					if config.ResourceVersion == "" {
						err = otherClient.Get().
							Namespace(config.Namespace).
							Name(config.Name).
							Resource(resource.Name).
							Do().
							Into(&current)
						if err == nil && current.ResourceVersion != "" {
							config.ResourceVersion = current.ResourceVersion
						}
					}

					err = otherClient.Put().
						Namespace(config.Namespace).
						Name(config.Name).
						Resource(resource.Name).
						Body(&config).
						Do().
						Into(&updated)
					if err != nil {
						errs = multierror.Append(errs, err)
						continue
					}
					key := model.Key(config.Kind, config.Name, config.Namespace)
					fmt.Printf("Updated config %s to revision %v\n", key, updated.ResourceVersion)
				}
				if errs != nil {
					return errs
				}
			}

			return nil
		},
	}

	getCmd = &cobra.Command{
		Use:        "get <type> [<name>]",
		Deprecated: "Use `kubectl get` instead (see https://kubernetes.io/docs/tasks/tools/install-kubectl)",
		Short:      "Retrieve policies and rules",
		Example: `# List all virtual services
istioctl get virtualservices

# List all destination rules
istioctl get destinationrules

# Get a specific virtual service named bookinfo
istioctl get virtualservice bookinfo
`,
		RunE: func(c *cobra.Command, args []string) error {
			configClient, err := clientFactory()
			if err != nil {
				return err
			}
			if len(args) < 1 {
				c.Println(c.UsageString())
				return fmt.Errorf("specify the type of resource to get. Types are %v",
					strings.Join(supportedTypes(configClient), ", "))
			}

			getByName := len(args) > 1
			if getAllNamespaces && getByName {
				return errors.New("a resource cannot be retrieved by name across all namespaces")
			}

			var schemas collection.Schemas
			if !getByName && strings.EqualFold(args[0], "all") {
				schemas = configClient.Schemas()
			} else {
				typ, err := protoSchema(configClient, args[0])
				if err != nil {
					c.Println(c.UsageString())
					return err
				}
				schemas = collection.SchemasFor(typ)
			}

			var ns string
			if getAllNamespaces {
				ns = v1.NamespaceAll
			} else {
				ns = handlers.HandleNamespace(namespace, defaultNamespace)
			}

			var errs error
			var configs []model.Config
			if getByName {
				config := configClient.Get(schemas.All()[0].Resource().GroupVersionKind(), args[1], ns)
				if config != nil {
					configs = append(configs, *config)
				}
			} else {
				for _, s := range schemas.All() {
					kind := s.Resource().GroupVersionKind()
					typeConfigs, err := configClient.List(kind, ns)
					if err == nil {
						configs = append(configs, typeConfigs...)
					} else {
						if mustList[kind.Kind] {
							errs = multierror.Append(errs, multierror.Prefix(err, fmt.Sprintf("Can't list %v:", kind)))
						}
					}
				}
			}

			if len(configs) == 0 {
				c.Println("No resources found.")
				return errs
			}

			var outputters = map[string]func(io.Writer, model.ConfigStore, []model.Config){
				"yaml":  printYamlOutput,
				"short": printShortOutput,
			}

			if outputFunc, ok := outputters[outputFormat]; ok {
				outputFunc(c.OutOrStdout(), configClient, configs)
			} else {
				return fmt.Errorf("unknown output format %v. Types are yaml|short", outputFormat)
			}

			return errs
		},

		ValidArgs:  configTypeResourceNames(collections.Pilot),
		ArgAliases: configTypePluralResourceNames(collections.Pilot),
	}

	deleteCmd = &cobra.Command{
		Use:        "delete <type> <name> [<name2> ... <nameN>]",
		Deprecated: "Use `kubectl delete` instead (see https://kubernetes.io/docs/tasks/tools/install-kubectl)",
		Short:      "Delete policies or rules",
		Example: `# Delete a rule using the definition in example-routing.yaml.
istioctl delete -f example-routing.yaml

# Delete the virtual service bookinfo
istioctl delete virtualservice bookinfo
`,
		RunE: func(c *cobra.Command, args []string) error {
			configClient, errs := clientFactory()
			if errs != nil {
				return errs
			}
			// If we did not receive a file option, get names of resources to delete from command line
			if file == "" {
				if len(args) < 2 {
					c.Println(c.UsageString())
					return fmt.Errorf("provide configuration type and name or -f option")
				}
				typ, err := protoSchema(configClient, args[0])
				if err != nil {
					return err
				}
				ns := handlers.HandleNamespace(namespace, defaultNamespace)
				for i := 1; i < len(args); i++ {
					if err := configClient.Delete(typ.Resource().GroupVersionKind(), args[i], ns); err != nil {
						errs = multierror.Append(errs,
							fmt.Errorf("cannot delete %s: %v", args[i], err))
					} else {
						c.Printf("Deleted config: %v %v\n", args[0], args[i])
					}
				}
				return errs
			}

			// As we did get a file option, make sure the command line did not include any resources to delete
			if len(args) != 0 {
				c.Println(c.UsageString())
				return fmt.Errorf("delete takes no arguments when the file option is used")
			}
			varr, others, err := readInputs()
			if err != nil {
				return err
			}
			if len(varr) == 0 && len(others) == 0 {
				return errors.New("nothing to delete")
			}
			for _, config := range varr {
				if config.Namespace, err = handlers.HandleNamespaces(config.Namespace, namespace, defaultNamespace); err != nil {
					return err
				}

				// compute key if necessary
				if err = configClient.Delete(config.GroupVersionKind(), config.Name, config.Namespace); err != nil {
					errs = multierror.Append(errs, fmt.Errorf("cannot delete %s: %v", config.Key(), err))
				} else {
					c.Printf("Deleted config: %v\n", config.Key())
				}
			}
			if errs != nil {
				return errs
			}

			if len(others) > 0 {
				if err = preprocMixerConfig(others); err != nil {
					return err
				}
				otherClient, resources, oerr := prepareClientForOthers(others)
				if oerr != nil {
					return oerr
				}
				for _, config := range others {
					resource, ok := resources[config.Kind]
					if !ok {
						errs = multierror.Append(errs, fmt.Errorf("kind %s is not known", config.Kind))
						continue
					}
					err = otherClient.Delete().
						Namespace(config.Namespace).
						Resource(resource.Name).
						Name(config.Name).
						Do().
						Error()
					if err != nil {
						errs = multierror.Append(errs, fmt.Errorf("failed to delete: %v", err))
						continue
					}
					fmt.Printf("Deleted config: %s\n", model.Key(config.Kind, config.Name, config.Namespace))
				}
			}

			return errs
		},

		ValidArgs:  configTypeResourceNames(collections.Pilot),
		ArgAliases: configTypePluralResourceNames(collections.Pilot),
	}

	contextCmd = &cobra.Command{
		Use: "context-create --api-server http://<ip>:<port>",
		Deprecated: `Use kubectl instead (see https://kubernetes.io/docs/tasks/tools/install-kubectl), e.g.

	$ kubectl config set-context istio --cluster=istio
	$ kubectl config set-cluster istio --server=http://localhost:8080
	$ kubectl config use-context istio
`,
		Short: "Create a kubeconfig file suitable for use with istioctl in a non-Kubernetes environment",
		Example: `# Create a config file for the api server.
istioctl context-create --api-server http://127.0.0.1:8080
`,
		RunE: func(c *cobra.Command, args []string) error {
			if istioAPIServer == "" {
				c.Println(c.UsageString())
				return fmt.Errorf("specify the the Istio api server IP")
			}

			u, err := url.ParseRequestURI(istioAPIServer)
			if err != nil {
				c.Println(c.UsageString())
				return err
			}

			configAccess := clientcmd.NewDefaultPathOptions()
			// use specified kubeconfig file for the location of the config to create or modify
			configAccess.GlobalFile = kubeconfig

			// gets existing kubeconfig or returns new empty config
			config, err := configAccess.GetStartingConfig()
			if err != nil {
				return err
			}

			cluster, exists := config.Clusters[istioContext]
			if !exists {
				cluster = clientcmdapi.NewCluster()
			}
			cluster.Server = u.String()
			config.Clusters[istioContext] = cluster

			context, exists := config.Contexts[istioContext]
			if !exists {
				context = clientcmdapi.NewContext()
			}
			context.Cluster = istioContext
			config.Contexts[istioContext] = context

			contextSwitched := false
			if config.CurrentContext != "" && config.CurrentContext != istioContext {
				contextSwitched = true
			}
			config.CurrentContext = istioContext
			if err = clientcmd.ModifyConfig(configAccess, *config, false); err != nil {
				return err
			}

			if contextSwitched {
				fmt.Printf("kubeconfig context switched to %q\n", istioContext)
			}
			fmt.Println("Context created")
			return nil
		},
	}
)

// The protoSchema is based on the kind (for example "virtualservice" or "destinationrule")
func protoSchema(configClient model.ConfigStore, typ string) (collection.Schema, error) {
	if strings.Contains(typ, "-") {
		return nil, fmt.Errorf("%q not recognized. Please use non-hyphenated resource name %q",
			typ, strings.ReplaceAll(typ, "-", ""))
	}

	for _, s := range configClient.Schemas().All() {
		switch strings.ToLower(typ) {
		case strings.ToLower(s.Resource().Kind()), strings.ToLower(s.Resource().Plural()):
			return s, nil
		}
	}
	return nil, fmt.Errorf("configuration type %s not found, the types are %v",
		typ, strings.Join(supportedTypes(configClient), ", "))
}

// readInputs reads multiple documents from the input and checks with the schema
func readInputs() ([]model.Config, []crd.IstioKind, error) {
	var reader io.Reader
	switch file {
	case "":
		return nil, nil, errors.New("filename not specified (see --filename or -f)")
	case "-":
		reader = os.Stdin
	default:
		var err error
		var in *os.File
		if in, err = os.Open(file); err != nil {
			return nil, nil, err
		}
		defer func() {
			if err = in.Close(); err != nil {
				log.Errorf("Error: close file from %s, %s", file, err)
			}
		}()
		reader = in
	}
	input, err := ioutil.ReadAll(reader)
	if err != nil {
		return nil, nil, err
	}
	return crd.ParseInputsWithoutValidation(string(input))
}

// Print a simple list of names
func printShortOutput(writer io.Writer, _ model.ConfigStore, configList []model.Config) {
	// Sort configList by Type
	sort.Slice(configList, func(i, j int) bool { return sortWeight[configList[i].Type] < sortWeight[configList[j].Type] })

	var w tabwriter.Writer
	w.Init(writer, 10, 4, 3, ' ', 0)
	prevType := ""
	var outputter func(model.Config, io.Writer)
	for _, c := range configList {
		if prevType != c.Type {
			if prevType != "" {
				// Place a newline between types when doing 'get all'
				_, _ = fmt.Fprintf(&w, "\n")
			}
			heading, ok := shortOutputHeadings[c.Type]
			if !ok {
				heading = unknownShortOutputHeading
			}
			_, _ = fmt.Fprintf(&w, "%s\n", heading)
			prevType = c.Type

			if outputter, ok = shortOutputters[c.Type]; !ok {
				outputter = printShortConfig
			}
		}

		outputter(c, &w)
	}
	_ = w.Flush()
}

func kindAsString(config model.Config) string {
	return fmt.Sprintf("%s.%s.%s",
		config.Type,
		config.Group,
		config.Version,
	)
}

func printShortConfig(config model.Config, w io.Writer) {
	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
		config.Name,
		kindAsString(config),
		config.Namespace,
		renderTimestamp(config.CreationTimestamp))
}

func printShortVirtualService(config model.Config, w io.Writer) {
	virtualService, ok := config.Spec.(*v1alpha3.VirtualService)
	if !ok {
		_, _ = fmt.Fprintf(w, "Not a virtualservice: %v", config)
		return
	}

	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%5d\t%4d\t%s\t%s\n",
		config.Name,
		strings.Join(virtualService.Gateways, ","),
		strings.Join(virtualService.Hosts, ","),
		len(virtualService.Http),
		len(virtualService.Tcp),
		config.Namespace,
		renderTimestamp(config.CreationTimestamp))
}

func printShortDestinationRule(config model.Config, w io.Writer) {
	destinationRule, ok := config.Spec.(*v1alpha3.DestinationRule)
	if !ok {
		_, _ = fmt.Fprintf(w, "Not a destinationrule: %v", config)
		return
	}

	subsets := make([]string, 0)
	for _, subset := range destinationRule.Subsets {
		subsets = append(subsets, subset.Name)
	}

	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
		config.Name,
		destinationRule.Host,
		strings.Join(subsets, ","),
		config.Namespace,
		renderTimestamp(config.CreationTimestamp))
}

func printShortServiceEntry(config model.Config, w io.Writer) {
	serviceEntry, ok := config.Spec.(*v1alpha3.ServiceEntry)
	if !ok {
		_, _ = fmt.Fprintf(w, "Not a serviceentry: %v", config)
		return
	}

	ports := make([]string, 0)
	for _, port := range serviceEntry.Ports {
		ports = append(ports, fmt.Sprintf("%s/%d", port.Protocol, port.Number))
	}

	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
		config.Name,
		strings.Join(serviceEntry.Hosts, ","),
		strings.Join(ports, ","),
		config.Namespace,
		renderTimestamp(config.CreationTimestamp))
}

func printShortGateway(config model.Config, w io.Writer) {
	gateway, ok := config.Spec.(*v1alpha3.Gateway)
	if !ok {
		_, _ = fmt.Fprintf(w, "Not a gateway: %v", config)
		return
	}

	// Determine the servers
	servers := make(map[string]bool)
	for _, server := range gateway.Servers {
		for _, host := range server.Hosts {
			servers[host] = true
		}
	}
	hosts := make([]string, 0)
	for host := range servers {
		hosts = append(hosts, host)
	}

	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
		config.Name, strings.Join(hosts, ","), config.Namespace,
		renderTimestamp(config.CreationTimestamp))
}

// Print as YAML
func printYamlOutput(writer io.Writer, configClient model.ConfigStore, configList []model.Config) {
	schema := configClient.Schemas()
	for _, config := range configList {
		s, exists := schema.FindByGroupVersionKind(config.GroupVersionKind())
		if !exists {
			log.Errorf("Unknown kind %q for %v", config.Type, config.Name)
			continue
		}
		obj, err := crd.ConvertConfig(s, config)
		if err != nil {
			log.Errorf("Could not decode %v: %v", config.Name, err)
			continue
		}
		bytes, err := yaml.Marshal(obj)
		if err != nil {
			log.Errorf("Could not convert %v to YAML: %v", config, err)
			continue
		}
		_, _ = fmt.Fprint(writer, string(bytes))
		_, _ = fmt.Fprintln(writer, "---")
	}
}

func newClient() (model.ConfigStore, error) {
	return controller.NewClient(kubeconfig, configContext, collections.Pilot,
		"", &model.DisabledLedger{})
}

func supportedTypes(configClient model.ConfigStore) []string {
	return configClient.Schemas().Kinds()
}

func preprocMixerConfig(configs []crd.IstioKind) error {
	var err error
	for i, config := range configs {
		if configs[i].Namespace, err = handlers.HandleNamespaces(config.Namespace, namespace, defaultNamespace); err != nil {
			return err
		}
		if config.APIVersion == "" {
			configs[i].APIVersion = legacyIstioAPIGroupVersion.String()
		}
		// TODO: invokes the mixer validation webhook.
	}
	return nil
}

func restConfig() (config *rest.Config, err error) {
	config, err = kubecfg.BuildClientConfig(kubeconfig, configContext)

	if err != nil {
		return
	}

	config.GroupVersion = &legacyIstioAPIGroupVersion
	config.APIPath = "/apis"
	config.ContentType = runtime.ContentTypeJSON

	types := runtime.NewScheme()
	schemeBuilder := runtime.NewSchemeBuilder(
		func(scheme *runtime.Scheme) error {
			metav1.AddToGroupVersion(scheme, legacyIstioAPIGroupVersion)
			return nil
		})
	err = schemeBuilder.AddToScheme(types)
	config.NegotiatedSerializer = serializer.WithoutConversionCodecFactory{CodecFactory: serializer.NewCodecFactory(types)}
	return
}

func apiResources(config *rest.Config, configs []crd.IstioKind) (map[string]metav1.APIResource, error) {
	client, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, err
	}
	resources, err := client.ServerResourcesForGroupVersion(legacyIstioAPIGroupVersion.String())
	if err != nil {
		return nil, err
	}

	kindsSet := map[string]bool{}
	for _, config := range configs {
		if !kindsSet[config.Kind] {
			kindsSet[config.Kind] = true
		}
	}
	result := make(map[string]metav1.APIResource, len(kindsSet))
	for _, resource := range resources.APIResources {
		if kindsSet[resource.Kind] {
			result[resource.Kind] = resource
		}
	}
	return result, nil
}

func restClientForOthers(config *rest.Config) (*rest.RESTClient, error) {
	return rest.RESTClientFor(config)
}

func prepareClientForOthers(configs []crd.IstioKind) (*rest.RESTClient, map[string]metav1.APIResource, error) {
	restConfig, err := restConfig()
	if err != nil {
		return nil, nil, err
	}
	resources, err := apiResources(restConfig, configs)
	if err != nil {
		return nil, nil, err
	}
	client, err := restClientForOthers(restConfig)
	if err != nil {
		return nil, nil, err
	}
	return client, resources, nil
}

func configTypeResourceNames(schemas collection.Schemas) []string {
	all := schemas.All()
	resourceNames := make([]string, len(all))
	for _, s := range all {
		resourceNames = append(resourceNames, s.Resource().Kind())
	}
	return resourceNames
}

func configTypePluralResourceNames(schemas collection.Schemas) []string {
	all := schemas.All()
	resourceNames := make([]string, len(all))
	for _, s := range all {
		resourceNames = append(resourceNames, s.Resource().Plural())
	}
	return resourceNames
}

// renderTimestamp creates a human-readable age similar to docker and kubectl CLI output
func renderTimestamp(ts time.Time) string {
	if ts.IsZero() {
		return "<unknown>"
	}

	seconds := int(time.Since(ts).Seconds())
	if seconds < -2 {
		return fmt.Sprintf("<invalid>")
	} else if seconds < 0 {
		return fmt.Sprintf("0s")
	} else if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}

	minutes := int(time.Since(ts).Minutes())
	if minutes < 60 {
		return fmt.Sprintf("%dm", minutes)
	}

	hours := int(time.Since(ts).Hours())
	if hours < 24 {
		return fmt.Sprintf("%dh", hours)
	} else if hours < 365*24 {
		return fmt.Sprintf("%dd", hours/24)
	}
	return fmt.Sprintf("%dy", hours/24/365)
}

func init() {
	defaultContext := "istio"
	contextCmd.PersistentFlags().StringVar(&istioContext, "context", defaultContext,
		"Kubernetes configuration file context name")
	contextCmd.PersistentFlags().StringVar(&istioAPIServer, "api-server", "",
		"URL for Istio api server")

	postCmd.PersistentFlags().StringVarP(&file, "file", "f", "",
		"Input file with the content of the configuration objects (if not set, command reads from the standard input)")
	putCmd.PersistentFlags().AddFlag(postCmd.PersistentFlags().Lookup("file"))
	deleteCmd.PersistentFlags().AddFlag(postCmd.PersistentFlags().Lookup("file"))

	getCmd.PersistentFlags().StringVarP(&outputFormat, "output", "o", "short",
		"Output format. One of:yaml|short")
	getCmd.PersistentFlags().BoolVar(&getAllNamespaces, "all-namespaces", false,
		"If present, list the requested object(s) across all namespaces. Namespace in current "+
			"context is ignored even if specified with --namespace.")
}
