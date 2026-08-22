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

// Package controller reconciles CrossService objects.
package controller

import (
	"fmt"
	"net/netip"
	"strings"

	netv1alpha1 "github.com/obaydullahmhs/cross-cluster-service/api/v1alpha1"
)

// validateSpec covers the rules CRD validation cannot express.
//
// Most cross-field checking already happens in CEL, which is cheaper and shows
// up at kubectl apply time. What is left is the handful of things CEL cannot
// do on the minimum supported apiserver -- notably parsing an IP, since the
// isIP() extension is not available before 1.31.
func validateSpec(xsvc *netv1alpha1.CrossService) error {
	src := &xsvc.Spec.Source

	if err := validatePortNames(xsvc.Spec.Ports); err != nil {
		return err
	}

	switch src.Type {
	case netv1alpha1.SourceTypeStatic:
		if src.Static == nil {
			return fmt.Errorf("static source has no static config")
		}
		for _, a := range src.Static.Addresses {
			if _, err := netip.ParseAddr(a); err != nil {
				return fmt.Errorf("static address %q is not a valid IP address", a)
			}
		}
	case netv1alpha1.SourceTypeDNS:
		if src.DNS == nil {
			return fmt.Errorf("dns source has no dns config")
		}
		if src.DNS.RecordType == netv1alpha1.DNSRecordTypeSRV &&
			src.DNS.SRVPortName == "" && len(xsvc.Spec.Ports) > 1 {
			return fmt.Errorf("srvPortName is required when an SRV source has more than one port")
		}
	}
	return nil
}

// validatePortNames enforces the parts of IANA_SVC_NAME that the CRD pattern
// cannot, and re-checks uniqueness so that a CR admitted by an older CRD
// revision still fails loudly rather than producing a broken slice.
func validatePortNames(ports []netv1alpha1.CrossServicePort) error {
	seen := map[string]bool{}
	for _, p := range ports {
		if seen[p.Name] {
			return fmt.Errorf("duplicate port name %q", p.Name)
		}
		seen[p.Name] = true

		if p.Name == "" {
			if len(ports) > 1 {
				return fmt.Errorf("port name is required when more than one port is defined")
			}
			continue
		}
		if len(p.Name) > 15 {
			return fmt.Errorf("port name %q exceeds 15 characters", p.Name)
		}
		if !strings.ContainsFunc(p.Name, func(r rune) bool { return r >= 'a' && r <= 'z' }) {
			return fmt.Errorf("port name %q must contain at least one letter", p.Name)
		}
		if strings.Contains(p.Name, "--") {
			return fmt.Errorf("port name %q must not contain adjacent hyphens", p.Name)
		}
	}
	return nil
}
