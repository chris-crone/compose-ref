/*
   Copyright 2020 The Compose Specification Authors.

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
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v2"

	"github.com/compose-spec/compose-go/loader"
	compose "github.com/compose-spec/compose-go/types"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/client"
	"github.com/docker/go-units"
	commandLine "github.com/urfave/cli/v2"

	"github.com/compose-spec/compose-ref/internal"
)

const banner = `
 .---.
(     )
 )@ @(
//|||\\

`

func main() {
	var file string
	var project string

	fmt.Print(banner)
	app := &commandLine.App{
		Name:  "compose-ref",
		Usage: "Reference Compose Specification implementation",
		Flags: []commandLine.Flag{
			&commandLine.StringFlag{
				Name:        "file",
				Aliases:     []string{"f"},
				Value:       "compose.yaml",
				Usage:       "Load Compose file `FILE`",
				Destination: &file,
			},
			&commandLine.StringFlag{
				Name:        "project-name",
				Aliases:     []string{"n"},
				Value:       "",
				Usage:       "Set project name `NAME` (default: Compose file's folder name)",
				Destination: &project,
			},
		},
		Commands: []*commandLine.Command{
			{
				Name:  "up",
				Usage: "Create and start application services",
				Action: func(c *commandLine.Context) error {
					config, err := load(file)
					if err != nil {
						return err
					}
					project, err = getProject(project, file)
					if err != nil {
						return err
					}
					return doUp(project, config)
				},
			},
			{
				Name:  "down",
				Usage: "Stop services created by `up`",
				Action: func(c *commandLine.Context) error {
					config, err := load(file)
					if err != nil {
						return err
					}
					project, err = getProject(project, file)
					if err != nil {
						return err
					}
					return doDown(project, config)
				},
			},
		},
	}

	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(err)
	}
}

func getProject(project string, file string) (string, error) {
	if project == "" {
		abs, err := filepath.Abs(file)
		if err != nil {
			return "", err
		}
		project = filepath.Base(filepath.Dir(abs))
	}
	return project, nil
}

func getClient() (*client.Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return nil, err
	}
	cli.NegotiateAPIVersion(context.Background())
	return cli, nil
}

func doUp(project string, config *compose.Config) error {
	cli, err := getClient()
	if err != nil {
		return err
	}

	prjDir, err := filepath.Abs(filepath.Dir(config.Filename))
	if err != nil {
		return err
	}

	networks, err := internal.GetNetworksFromConfig(cli, project, config)
	if err != nil {
		return err
	}

	err = internal.GetVolumesFromConfig(cli, project, config)
	if err != nil {
		return err
	}

	err = internal.GetConfigsFromConfig(prjDir, config)
	if err != nil {
		return err
	}

	err = internal.GetSecretsFromConfig(prjDir, config)
	if err != nil {
		return err
	}

	observedState, err := internal.CollectContainers(cli, project)
	if err != nil {
		return err
	}

	err = config.WithServices(nil, func(service compose.ServiceConfig) error {
		containers := observedState[service.Name]
		delete(observedState, service.Name)

		// If no container exists for the service yet, then we just need to create them
		if len(containers) == 0 {
			return createService(cli, project, prjDir, service, networks)
		}

		// We compare container config stored as plain yaml in a label with expected one
		b, err := yaml.Marshal(service)
		if err != nil {
			return err
		}
		expected := string(b)

		diverged := false
		for _, cntr := range containers {
			config := cntr.Labels[internal.LabelConfig]
			if config != expected {
				diverged = true
				break
			}
		}

		if !diverged {
			// Existing containers are up-to-date with the Compose file configuration, so just keep them running
			return nil
		}

		// Some containers exist for service but with an obsolete configuration. We need to replace them
		err = internal.RemoveContainers(cli, containers)
		if err != nil {
			return err
		}
		return createService(cli, project, prjDir, service, networks)
	})

	if err != nil {
		return err
	}

	// Remaining containers in observed state don't have a matching service in Compose file => orphaned to be removed
	for _, orphaned := range observedState {
		err = internal.RemoveContainers(cli, orphaned)
		if err != nil {
			return err
		}
	}
	return nil
}

func createService(cli *client.Client, project string, prjDir string, s compose.ServiceConfig, networks map[string]string) error {
	ctx := context.Background()

	var shmSize int64
	if s.ShmSize != "" {
		v, err := units.RAMInBytes(s.ShmSize)
		if err != nil {
			return err
		}
		shmSize = v
	}

	labels := map[string]string{}
	for k, v := range s.Labels {
		labels[k] = v
	}
	labels[internal.LabelProject] = project
	labels[internal.LabelService] = s.Name

	b, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	labels[internal.LabelConfig] = string(b)

	fmt.Printf("Creating container for service %s ... ", s.Name)
	networkMode := internal.NetworkMode(project, s, networks)
	mounts, err := internal.CreateContainerMounts(s, prjDir)
	if err != nil {
		return err
	}
	configMounts, err := internal.CreateContainerConfigMounts(s, prjDir)
	if err != nil {
		return err
	}
	secretsMounts, err := internal.CreateContainerSecretMounts(s, prjDir)
	if err != nil {
		return err
	}
	mounts = append(mounts, configMounts...)
	mounts = append(mounts, secretsMounts...)
	create, err := cli.ContainerCreate(ctx,
		&container.Config{
			Hostname:        s.Hostname,
			Domainname:      s.DomainName,
			User:            s.User,
			Tty:             s.Tty,
			OpenStdin:       s.StdinOpen,
			Cmd:             strslice.StrSlice(s.Command),
			Image:           s.Image,
			Labels:          labels,
			WorkingDir:      s.WorkingDir,
			Entrypoint:      strslice.StrSlice(s.Entrypoint),
			NetworkDisabled: s.NetworkMode == "disabled",
			MacAddress:      s.MacAddress,
			StopSignal:      s.StopSignal,
			ExposedPorts:    internal.ExposedPorts(s.Ports),
		},
		&container.HostConfig{
			NetworkMode:    networkMode,
			RestartPolicy:  container.RestartPolicy{Name: s.Restart},
			CapAdd:         s.CapAdd,
			CapDrop:        s.CapDrop,
			DNS:            s.DNS,
			DNSSearch:      s.DNSSearch,
			ExtraHosts:     s.ExtraHosts,
			IpcMode:        container.IpcMode(s.Ipc),
			Links:          s.Links,
			Mounts:         mounts,
			PidMode:        container.PidMode(s.Pid),
			Privileged:     s.Privileged,
			ReadonlyRootfs: s.ReadOnly,
			SecurityOpt:    s.SecurityOpt,
			UsernsMode:     container.UsernsMode(s.UserNSMode),
			ShmSize:        shmSize,
			Sysctls:        s.Sysctls,
			Isolation:      container.Isolation(s.Isolation),
			Init:           s.Init,
			PortBindings:   internal.BuildContainerPortBindingsOptions(s),
		},
		internal.BuildDefaultNetworkConfig(s, networkMode),
		"")
	if err != nil {
		return err
	}
	err = internal.ConnectContainerToNetworks(ctx, cli, s, create.ID, networks)
	if err != nil {
		return err
	}
	err = cli.ContainerStart(ctx, create.ID, types.ContainerStartOptions{})
	if err != nil {
		return err
	}
	fmt.Println(create.ID)
	return nil
}

func doDown(project string, config *compose.Config) error {
	cli, err := getClient()
	if err != nil {
		return nil
	}
	err = removeServices(cli, project)
	if err != nil {
		return err
	}
	err = internal.RemoveVolumes(cli, project)
	if err != nil {
		return err
	}
	err = internal.RemoveNetworks(cli, project)
	if err != nil {
		return err
	}
	return nil
}

func removeServices(cli *client.Client, project string) error {
	containers, err := internal.CollectContainers(cli, project)
	if err != nil {
		return err
	}

	for _, replicaList := range containers {
		err = internal.RemoveContainers(cli, replicaList)
		if err != nil {
			return err
		}
	}
	return nil
}

func load(file string) (*compose.Config, error) {
	b, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, err
	}
	config, err := loader.ParseYAML(b)
	if err != nil {
		return nil, err
	}
	var files []compose.ConfigFile
	files = append(files, compose.ConfigFile{Filename: file, Config: config})
	return loader.Load(compose.ConfigDetails{
		WorkingDir:  ".",
		ConfigFiles: files,
	})
}
