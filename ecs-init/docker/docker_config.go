// Copyright 2017-2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package docker

import (
	"fmt"
	"os"

	"github.com/aws/amazon-ecs-agent/ecs-init/config"
	"github.com/cihub/seelog"
	ctrdapparmor "github.com/containerd/containerd/pkg/apparmor"
	godocker "github.com/fsouza/go-dockerclient"
)

// getPlatformSpecificEnvVariables gets a map of environment variable key-value
// pairs to set in the Agent's container config
// The ECS_ENABLE_TASK_ENI flag is only set for Amazon Linux AMI
func getPlatformSpecificEnvVariables() map[string]string {
	return map[string]string{
		"ECS_ENABLE_TASK_ENI":                       "true",
		"ECS_ENABLE_AWSLOGS_EXECUTIONROLE_OVERRIDE": "true",
	}
}

// createHostConfig creates the host config for the ECS Agent container
// It mounts leases and pid file directories when built for Amazon Linux AMI
func createHostConfig(binds []string) *godocker.HostConfig {
	binds = append(binds,
		config.ProcFS+":"+hostProcDir+readOnly,
		iptablesUsrLibDir+":"+iptablesUsrLibDir+readOnly,
		iptablesLibDir+":"+iptablesLibDir+readOnly,
		iptablesUsrLib64Dir+":"+iptablesUsrLib64Dir+readOnly,
		iptablesLib64Dir+":"+iptablesLib64Dir+readOnly,
		iptablesExecutableHostDir+":"+iptablesExecutableContainerDir+readOnly,
		iptablesAltDir+":"+iptablesAltDir+readOnly,
		iptablesLegacyDir+":"+iptablesLegacyDir+readOnly,
		lsblkDir+":"+lsblkDir,
	)
	binds = append(binds, getNsenterBinds(config.OsStat)...)
	binds = append(binds, getModInfoBinds(config.OsStat)...)

	logConfig := config.AgentDockerLogDriverConfiguration()

	var caps []string
	if !config.RunningInExternal() {
		// CapNetAdmin and CapSysAdmin are needed for running task in awsvpc network mode.
		// This network mode is (at least currently) not supported in external environment,
		// hence not adding them in that case.
		caps = []string{CapNetAdmin, CapSysAdmin}
	}

	hostConfig := &godocker.HostConfig{
		LogConfig:   logConfig,
		Binds:       binds,
		NetworkMode: networkMode,
		UsernsMode:  usernsMode,
		CapAdd:      caps,
	}

	// Task ENI feature requires agent to be "init" process, so set it if docker API
	// version is new enough for this feature.
	if dockerVersionCompare(dockerAPIVersion, enableTaskENIDockerClientAPIVersion) >= 0 {
		hostConfig.Init = true
	}

	if ctrdapparmor.HostSupports() {
		hostConfig.SecurityOpt = []string{fmt.Sprintf("apparmor:%s", config.ECSAgentAppArmorProfileName())}
	}

	if config.RunPrivileged() {
		hostConfig.Privileged = true
	}

	// Set the PID mode to host if ECS_AGENT_PID_NAMESPACE_HOST env variable is set to true
	if config.IsECSAgentPIDNamespaceHostEnabled() {
		hostConfig.PidMode = "host"
	}

	return hostConfig
}

// Returns nsenter bind as a slice if nsenter is available on the host.
// Returns an empty slice otherwise.
func getNsenterBinds(statFn func(string) (os.FileInfo, error)) []string {
	binds := []string{}
	if _, err := statFn(nsEnterDir); err == nil {
		binds = append(binds, nsEnterDir+":"+nsEnterDir)
	} else {
		seelog.Warnf("nsenter not found at %s, skip binding it to Agent container: %v",
			nsEnterDir, err)
	}
	return binds
}

// Returns modinfo bind as a slice if modinfo is available on the host.
// Otherwise, it will return an empty slice.
func getModInfoBinds(statFn func(string) (os.FileInfo, error)) []string {
	binds := []string{}
	modInfoPathLocations := []string{
		modInfoSbinDir,
		modInfoUsrSbinDir,
	}
	for _, path := range modInfoPathLocations {
		if _, err := statFn(path); err == nil {
			seelog.Debugf("modinfo found at %s", path)
			binds = append(binds, path+":"+path)
			break
		} else {
			seelog.Infof("modinfo not found at %s, skip binding it to Agent container: %v",
				path, err)
		}
	}
	return binds
}
