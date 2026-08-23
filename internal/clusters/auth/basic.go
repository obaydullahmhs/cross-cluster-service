/*
Copyright 2026.

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

package auth

import (
	"context"
	"fmt"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
)

// buildInCluster uses the controller's own ServiceAccount against the local
// apiserver.
func buildInCluster() (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("building in-cluster config: %w", err)
	}
	return cfg, nil
}

// buildToken presents a bearer token read from a Secret.
//
// The token is set directly rather than wrapped in a TokenSource because a
// Kubernetes ServiceAccount token has no refresh protocol: rotation happens by
// the Secret changing, which the controller watches and which changes this
// connection's fingerprint. That is the same outcome I15 asks for -- no
// connection outliving its credential -- reached by the mechanism this
// credential actually has.
func (b *Builder) buildToken(
	ctx context.Context,
	access *netv1alpha1.ClusterAccess,
	fp *fingerprint,
) (*rest.Config, error) {
	cfg := access.Token
	token, err := b.Secrets.Value(ctx, cfg.SecretRef, "token")
	if err != nil {
		return nil, err
	}

	tlsCfg, err := b.tlsFrom(ctx, access, nil, fp)
	if err != nil {
		return nil, err
	}

	fp.add("server", cfg.Server)
	fp.addBytes("token", token)

	return &rest.Config{
		Host:            cfg.Server,
		BearerToken:     string(token),
		TLSClientConfig: tlsCfg,
	}, nil
}

// buildKubeconfig reads a full kubeconfig from a Secret.
func (b *Builder) buildKubeconfig(
	ctx context.Context,
	access *netv1alpha1.ClusterAccess,
	fp *fingerprint,
) (*rest.Config, error) {
	cfg := access.Kubeconfig

	// "value" first, then "config": the former is what most operators write,
	// the latter what kubectl users expect.
	raw, err := b.Secrets.Value(ctx, cfg.SecretRef, "value", "config")
	if err != nil {
		return nil, err
	}

	overrides := &clientcmd.ConfigOverrides{}
	if cfg.Context != "" {
		overrides.CurrentContext = cfg.Context
	}

	clientCfg, err := clientcmd.NewClientConfigFromBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing kubeconfig from secret %s: %w", cfg.SecretRef.Name, err)
	}
	if cfg.Context != "" {
		clientCfg = clientcmd.NewNonInteractiveClientConfig(
			*mustRawConfig(raw), cfg.Context, overrides, nil)
	}

	restCfg, err := clientCfg.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("building rest config from kubeconfig secret %s: %w", cfg.SecretRef.Name, err)
	}

	// An exec credential plugin smuggled in through a kubeconfig is the same
	// arbitrary code execution the ExecPlugin access type is gated on, so it
	// gets the same gate rather than a free pass.
	if restCfg.ExecProvider != nil && !b.Options.AllowExecCredentials {
		return nil, &ErrNotPermitted{
			Reason: "kubeconfig uses an exec credential plugin, which requires --allow-exec-credentials",
		}
	}

	if restCfg.Insecure && !b.Options.AllowInsecureTLS {
		return nil, &ErrNotPermitted{
			Reason: "kubeconfig disables TLS verification, which requires --allow-insecure-tls",
		}
	}

	fp.addBytes("kubeconfig", raw)
	fp.add("context", cfg.Context)
	return restCfg, nil
}

func mustRawConfig(raw []byte) *clientcmdapi.Config {
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return nil
	}
	return cfg
}
