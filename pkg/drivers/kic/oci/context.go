/*
Copyright 2024 The Kubernetes Authors All rights reserved.

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

package oci

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/context/docker"
	"github.com/docker/cli/cli/context/store"
	"github.com/pkg/errors"
	"k8s.io/klog/v2"
)

// ContextInfo contains information about the current Docker context
type ContextInfo struct {
	Name     string
	Host     string
	IsRemote bool
	IsSSH    bool
	TLSData  *store.EndpointTLSData
}

// GetCurrentContext returns information about the current Docker context
func GetCurrentContext() (*ContextInfo, error) {
	// Check if DOCKER_HOST is explicitly set
	if dockerHost := os.Getenv("DOCKER_HOST"); dockerHost != "" {
		return parseDockerHost(dockerHost, "environment")
	}

	// Check DOCKER_CONTEXT environment variable
	currentContext := os.Getenv("DOCKER_CONTEXT")
	if currentContext == "" {
		// Load from Docker config file
		dockerConfigDir := config.Dir()
		if _, err := os.Stat(dockerConfigDir); err != nil {
			if !os.IsNotExist(err) {
				return nil, errors.Wrap(err, "checking docker config directory")
			}
			// No config directory, assume default local context
			return &ContextInfo{
				Name:     "default",
				Host:     "",
				IsRemote: false,
				IsSSH:    false,
			}, nil
		}

		cf, err := config.Load(dockerConfigDir)
		if err != nil {
			return nil, errors.Wrap(err, "loading docker config")
		}
		currentContext = cf.CurrentContext
	}

	if currentContext == "" || currentContext == "default" {
		// Default context - local Docker daemon
		return &ContextInfo{
			Name:     "default",
			Host:     "",
			IsRemote: false,
			IsSSH:    false,
		}, nil
	}

	// Load context from store
	storeConfig := store.NewConfig(
		func() interface{} { return &docker.EndpointMeta{} },
		store.EndpointTypeGetter(docker.DockerEndpoint, func() interface{} { return &docker.EndpointMeta{} }),
	)
	st := store.New(config.ContextStoreDir(), storeConfig)
	
	md, err := st.GetMetadata(currentContext)
	if err != nil {
		return nil, errors.Wrapf(err, "getting metadata for context %q", currentContext)
	}

	dockerEP, ok := md.Endpoints[docker.DockerEndpoint]
	if !ok {
		return nil, errors.Errorf("context %q does not have a Docker endpoint", currentContext)
	}

	dockerEPMeta, ok := dockerEP.(docker.EndpointMeta)
	if !ok {
		return nil, errors.Errorf("expected docker.EndpointMeta, got %T", dockerEP)
	}

	info := &ContextInfo{
		Name: currentContext,
		Host: dockerEPMeta.Host,
	}

	if dockerEPMeta.Host != "" {
		var err error
		info.IsRemote, info.IsSSH, err = parseHostInfo(dockerEPMeta.Host)
		if err != nil {
			return nil, errors.Wrapf(err, "parsing host info for context %q", currentContext)
		}
	}

	// Load TLS data if available
	if info.IsRemote && !info.IsSSH {
		// For now, we'll skip loading TLS data - this would need proper implementation
		// based on the actual Docker CLI API
		klog.V(3).Infof("TLS data loading not implemented for remote context %q", currentContext)
	}

	return info, nil
}

// parseDockerHost parses a DOCKER_HOST value and returns context info
func parseDockerHost(dockerHost, source string) (*ContextInfo, error) {
	isRemote, isSSH, err := parseHostInfo(dockerHost)
	if err != nil {
		return nil, errors.Wrapf(err, "parsing DOCKER_HOST from %s", source)
	}

	return &ContextInfo{
		Name:     fmt.Sprintf("%s-host", source),
		Host:     dockerHost,
		IsRemote: isRemote,
		IsSSH:    isSSH,
	}, nil
}

// parseHostInfo determines if a Docker host is remote and uses SSH
func parseHostInfo(host string) (isRemote bool, isSSH bool, err error) {
	if host == "" {
		return false, false, nil
	}

	// Parse the URL
	u, err := url.Parse(host)
	if err != nil {
		return false, false, errors.Wrapf(err, "parsing host URL %q", host)
	}

	switch u.Scheme {
	case "ssh":
		return true, true, nil
	case "tcp", "https":
		// Check if this is actually a remote host or localhost
		hostname := u.Hostname()
		isLocal := hostname == "localhost" || hostname == "127.0.0.1" || hostname == "::1"
		return !isLocal, false, nil
	case "unix":
		// Unix socket is always local
		return false, false, nil
	case "npipe":
		// Named pipe (Windows) is always local
		return false, false, nil
	default:
		// Unknown scheme, assume remote
		klog.Warningf("Unknown Docker host scheme %q, assuming remote", u.Scheme)
		return true, false, nil
	}
}

// IsRemoteDockerContext checks if the current Docker context points to a remote daemon
func IsRemoteDockerContext() bool {
	ctx, err := GetCurrentContext()
	if err != nil {
		klog.Warningf("Error getting Docker context: %v", err)
		return false
	}
	return ctx.IsRemote
}

// IsSSHDockerContext checks if the current Docker context uses SSH
func IsSSHDockerContext() bool {
	ctx, err := GetCurrentContext()
	if err != nil {
		klog.Warningf("Error getting Docker context: %v", err)
		return false
	}
	return ctx.IsSSH
}

// ValidateRemoteDockerContext validates that a remote Docker context is properly configured
func ValidateRemoteDockerContext() error {
	ctx, err := GetCurrentContext()
	if err != nil {
		return errors.Wrap(err, "getting current Docker context")
	}

	if !ctx.IsRemote {
		return nil // Local context is always valid
	}

	if ctx.IsSSH {
		return validateSSHContext(ctx)
	}

	return validateTCPContext(ctx)
}

// validateSSHContext validates an SSH-based Docker context
func validateSSHContext(ctx *ContextInfo) error {
	if ctx.Host == "" {
		return errors.New("SSH Docker context has no host specified")
	}

	u, err := url.Parse(ctx.Host)
	if err != nil {
		return errors.Wrapf(err, "parsing SSH host %q", ctx.Host)
	}

	if u.User == nil || u.User.Username() == "" {
		return errors.New("SSH Docker context must specify a username")
	}

	if u.Hostname() == "" {
		return errors.New("SSH Docker context must specify a hostname")
	}

	// Additional SSH validation could be added here
	// e.g., checking SSH key availability, testing connection

	return nil
}

// validateTCPContext validates a TCP-based Docker context
func validateTCPContext(ctx *ContextInfo) error {
	if ctx.Host == "" {
		return errors.New("TCP Docker context has no host specified")
	}

	u, err := url.Parse(ctx.Host)
	if err != nil {
		return errors.Wrapf(err, "parsing TCP host %q", ctx.Host)
	}

	if u.Hostname() == "" {
		return errors.New("TCP Docker context must specify a hostname")
	}

	// For HTTPS/TLS connections, we should have TLS data
	if strings.HasPrefix(ctx.Host, "https://") || u.Scheme == "tcp" {
		if ctx.TLSData == nil {
			klog.Warningf("TCP Docker context %q may need TLS configuration", ctx.Name)
		}
	}

	return nil
}

// GetContextEnvironment returns environment variables for the current Docker context
func GetContextEnvironment() (map[string]string, error) {
	ctx, err := GetCurrentContext()
	if err != nil {
		return nil, errors.Wrap(err, "getting current Docker context")
	}

	env := make(map[string]string)

	if ctx.Host != "" {
		env["DOCKER_HOST"] = ctx.Host
	}

	if ctx.TLSData != nil {
		env["DOCKER_TLS_VERIFY"] = "1"
		// TLS cert path would need to be written to a temporary location
		// This is a simplified version - in practice, you'd extract and write the certificates
	}

	return env, nil
}

// SetupAPIServerTunnel sets up SSH tunnel for API server access if needed
func SetupAPIServerTunnel(apiServerPort int) (localEndpoint string, cleanup func(), err error) {
	ctx, err := GetCurrentContext()
	if err != nil {
		return "", nil, errors.Wrap(err, "getting current Docker context")
	}

	if !ctx.IsRemote || !ctx.IsSSH {
		// No tunnel needed for local or non-SSH contexts
		return "", func() {}, nil
	}

	klog.Infof("Setting up SSH tunnel for API server access (remote port %d)", apiServerPort)

	endpoint, err := GetAPIServerTunnelEndpoint(ctx, apiServerPort)
	if err != nil {
		return "", nil, errors.Wrap(err, "creating API server tunnel")
	}

	cleanup = func() {
		tm := GetTunnelManager()
		// Find and stop the tunnel for this API server port
		for key := range tm.GetActiveTunnels() {
			if strings.Contains(key, fmt.Sprintf(":%d->", apiServerPort)) {
				tm.StopTunnel(key)
				break
			}
		}
	}

	return endpoint, cleanup, nil
}

// CleanupAllTunnels stops all active SSH tunnels
func CleanupAllTunnels() {
	tm := GetTunnelManager()
	tm.StopAllTunnels()
}