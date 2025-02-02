/*
Copyright 2015 The Kubernetes Authors All rights reserved.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"github.com/ghodss/yaml"

	"github.com/kubernetes/deployment-manager/expandybird/expander"
	"github.com/kubernetes/deployment-manager/manager/manager"
	"github.com/kubernetes/deployment-manager/registry"
	"github.com/kubernetes/deployment-manager/util"

	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

var (
	deployment_name   = flag.String("name", "", "Name of deployment, used for deploy and update commands (defaults to template name)")
	stdin             = flag.Bool("stdin", false, "Reads a configuration from the standard input")
	properties        = flag.String("properties", "", "Properties to use when deploying a template (e.g., --properties k1=v1,k2=v2)")
	template_registry = flag.String("registry", "kubernetes/deployment-manager/templates", "Github based template registry (owner/repo[/path])")
	service           = flag.String("service", "http://localhost:8001/api/v1/proxy/namespaces/dm/services/manager-service:manager", "URL for deployment manager")
	binary            = flag.String("binary", "../expandybird/expansion/expansion.py", "Path to template expansion binary")
	timeout           = flag.Int("timeout", 10, "Time in seconds to wait for response")
)

var commands = []string{
	"expand \t\t\t Expands the supplied configuration(s)",
	"deploy \t\t\t Deploys the named template or the supplied configuration(s)",
	"list \t\t\t Lists the deployments in the cluster",
	"get \t\t\t Retrieves the supplied deployment",
	"delete \t\t\t Deletes the supplied deployment",
	"update \t\t\t Updates a deployment using the supplied configuration(s)",
	"deployed-types \t\t Lists the types deployed in the cluster",
	"deployed-instances \t Lists the instances of the named type deployed in the cluster",
	"templates \t\t Lists the templates in a given template registry",
	"describe \t\t Describes the named template in a given template registry",
}

var usage = func() {
	message := "Usage: %s [<flags>] <command> (<template-name> | <deployment-name> | (<configuration> [<import1>...<importN>]))\n"
	fmt.Fprintf(os.Stderr, message, os.Args[0])
	fmt.Fprintln(os.Stderr, "Commands:")
	for _, command := range commands {
		fmt.Fprintln(os.Stderr, command)
	}

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Flags:")
	flag.PrintDefaults()
	fmt.Fprintln(os.Stderr)
	os.Exit(1)
}

func getGitRegistry() *registry.GithubRegistry {
	s := strings.Split(*template_registry, "/")
	if len(s) < 2 {
		log.Fatalf("invalid template registry: %s", *template_registry)
	}

	var path = ""
	if len(s) > 2 {
		path = strings.Join(s[2:], "/")
	}

	return registry.NewGithubRegistry(s[0], s[1], path)
}

func main() {
	flag.Parse()
	args := flag.Args()
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "No command supplied")
		usage()
	}

	if *stdin {
		fmt.Printf("reading from stdin is not yet implemented")
		os.Exit(0)
	}

	command := args[0]
	switch command {
	case "templates":
		git := getGitRegistry()
		templates, err := git.List()
		if err != nil {
			log.Fatalf("Cannot list %v", err)
		}

		fmt.Printf("Templates:\n")
		for _, t := range templates {
			fmt.Printf("%s:%s\n", t.Name, t.Version)
			downloadURL := getDownloadUrl(t)

			fmt.Printf("\tdownload URL: %s\n", downloadURL)
		}
	case "describe":
		describeType(args)
	case "expand":
		template := loadTemplate(args)
		callService("expand", "POST", "expand configuration", marshalTemplate(template))
	case "deploy":
		template := loadTemplate(args)
		action := fmt.Sprintf("deploy configuration named %s", template.Name)
		callService("deployments", "POST", action, marshalTemplate(template))
	case "list":
		callService("deployments", "GET", "list deployments", nil)
	case "get":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "No deployment name supplied")
			usage()
		}

		path := fmt.Sprintf("deployments/%s", args[1])
		action := fmt.Sprintf("get deployment named %s", args[1])
		callService(path, "GET", action, nil)
	case "delete":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "No deployment name supplied")
			usage()
		}

		path := fmt.Sprintf("deployments/%s", args[1])
		action := fmt.Sprintf("delete deployment named %s", args[1])
		callService(path, "DELETE", action, nil)
	case "update":
		template := loadTemplate(args)
		path := fmt.Sprintf("deployments/%s", template.Name)
		action := fmt.Sprintf("delete deployment named %s", template.Name)
		callService(path, "PUT", action, marshalTemplate(template))
	case "deployed-types":
		callService("types", "GET", "list deployed types", nil)
	case "deployed-instances":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "No type name supplied")
			usage()
		}

		tUrl := getTypeUrl(args[1])
		path := fmt.Sprintf("types/%s/instances", url.QueryEscape(tUrl))
		action := fmt.Sprintf("list deployed instances of type %s", tUrl)
		callService(path, "GET", action, nil)
	default:
		usage()
	}
}

func callService(path, method, action string, reader io.ReadCloser) {
	u := fmt.Sprintf("%s/%s", *service, path)

	resp := callHttp(u, method, action, reader)
	var prettyJSON bytes.Buffer
	if err := json.Indent(&prettyJSON, []byte(resp), "", "  "); err != nil {
		log.Fatalf("Failed to parse JSON response from service: %s", resp)
	}
	fmt.Println(prettyJSON.String())
}

func callHttp(path, method, action string, reader io.ReadCloser) string {
	request, err := http.NewRequest(method, path, reader)
	request.Header.Add("Content-Type", "application/json")

	client := http.Client{
		Timeout: time.Duration(time.Duration(*timeout) * time.Second),
	}

	response, err := client.Do(request)
	if err != nil {
		log.Fatalf("cannot %s: %s\n", action, err)
	}

	defer response.Body.Close()
	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		log.Fatalf("cannot %s: %s\n", action, err)
	}

	if response.StatusCode < http.StatusOK ||
		response.StatusCode >= http.StatusMultipleChoices {
		message := fmt.Sprintf("status code: %d status: %s : %s", response.StatusCode, response.Status, body)
		log.Fatalf("cannot %s: %s\n", action, message)
	}

	return string(body)
}

// describeType prints the schema for a type specified by either a
// template URL or a fully qualified registry type name (e.g.,
// <type-name>:<version>)
func describeType(args []string) {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "No type name or URL supplied")
		usage()
	}

	tUrl := getTypeUrl(args[1])
	schemaUrl := tUrl + ".schema"
	fmt.Println(callHttp(schemaUrl, "GET", "get schema for type ("+tUrl+")", nil))
}

func getTypeUrl(tName string) string {
	if util.IsHttpUrl(tName) {
		// User can pass raw URL to template.
		return tName
	}

	// User can pass registry type.
	t := getRegistryType(tName)
	if t == nil {
		log.Fatalf("Invalid type name, must be in the form \"<type-name>:<version>\": %s", tName)
	}

	return getDownloadUrl(*t)
}

func getDownloadUrl(t registry.Type) string {
	git := getGitRegistry()
	url, err := git.GetURL(t)
	if err != nil {
		log.Fatalf("Failed to fetch type information for \"%s:%s\": %s", t.Name, t.Version, err)
	}

	return url
}

func isHttp(t string) bool {
	return strings.HasPrefix(t, "http://") || strings.HasPrefix(t, "https://")
}

func loadTemplate(args []string) *expander.Template {
	var template *expander.Template
	var err error
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "No template name or configuration(s) supplied")
		usage()
	}

	if len(args) < 3 {
		if t := getRegistryType(args[1]); t != nil {
			template = buildTemplateFromType(*t)
		} else {
			template, err = expander.NewTemplateFromRootTemplate(args[1])
		}
	} else {
		template, err = expander.NewTemplateFromFileNames(args[1], args[2:])
	}

	if err != nil {
		log.Fatalf("cannot create configuration from supplied arguments: %s\n", err)
	}

	// Override name if set from flags.
	if *deployment_name != "" {
		template.Name = *deployment_name
	}

	return template
}

// TODO: needs better validation that this is actually a registry type.
func getRegistryType(fullType string) *registry.Type {
	tList := strings.Split(fullType, ":")
	if len(tList) != 2 {
		return nil
	}

	return &registry.Type{
		Name:    tList[0],
		Version: tList[1],
	}
}

func buildTemplateFromType(t registry.Type) *expander.Template {
	downloadURL := getDownloadUrl(t)

	props := make(map[string]interface{})
	if *properties != "" {
		plist := strings.Split(*properties, ",")
		for _, p := range plist {
			ppair := strings.Split(p, "=")
			if len(ppair) != 2 {
				log.Fatalf("--properties must be in the form \"p1=v1,p2=v2,...\": %s\n", p)
			}

			// support ints
			// TODO: needs to support other types.
			i, err := strconv.Atoi(ppair[1])
			if err != nil {
				props[ppair[0]] = ppair[1]
			} else {
				props[ppair[0]] = i
			}
		}
	}

	name := fmt.Sprintf("%s-%s", t.Name, t.Version)
	config := manager.Configuration{Resources: []*manager.Resource{&manager.Resource{
		Name:       name,
		Type:       downloadURL,
		Properties: props,
	}}}

	y, err := yaml.Marshal(config)
	if err != nil {
		log.Fatalf("error: %s\ncannot create configuration for deployment: %v\n", err, config)
	}

	return &expander.Template{
		Name:    name,
		Content: string(y),
		// No imports, as this is a single type from repository.
	}
}

func marshalTemplate(template *expander.Template) io.ReadCloser {
	j, err := json.Marshal(template)
	if err != nil {
		log.Fatalf("cannot deploy configuration %s: %s\n", template.Name, err)
	}

	return ioutil.NopCloser(bytes.NewReader(j))
}

func getRandomName() string {
	return fmt.Sprintf("manifest-%d", time.Now().UTC().UnixNano())
}
