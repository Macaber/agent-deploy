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

package main

import "testing"

func TestParseNamespaceRuleJSON(t *testing.T) {
	t.Run("empty value means no controller-wide exceptions", func(t *testing.T) {
		rules, err := parseNamespaceRuleJSON("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rules != nil {
			t.Fatalf("expected nil rules, got %+v", rules)
		}
	})

	t.Run("whitespace value means no controller-wide exceptions", func(t *testing.T) {
		rules, err := parseNamespaceRuleJSON("   \n\t ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rules != nil {
			t.Fatalf("expected nil rules, got %+v", rules)
		}
	})

	t.Run("valid rules", func(t *testing.T) {
		raw := `[{"namespace":"monitoring","podSelector":{"app.kubernetes.io/name":"otel-collector"},"ports":[4318]},{"namespace":"logging"}]`
		rules, err := parseNamespaceRuleJSON(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(rules) != 2 {
			t.Fatalf("expected 2 rules, got %d", len(rules))
		}
		if rules[0].Namespace != "monitoring" ||
			rules[0].PodSelector["app.kubernetes.io/name"] != "otel-collector" ||
			len(rules[0].Ports) != 1 || rules[0].Ports[0] != 4318 {
			t.Fatalf("unexpected first rule: %+v", rules[0])
		}
		if rules[1].Namespace != "logging" || rules[1].PodSelector != nil || rules[1].Ports != nil {
			t.Fatalf("unexpected second rule: %+v", rules[1])
		}
	})

	t.Run("malformed json is rejected", func(t *testing.T) {
		if _, err := parseNamespaceRuleJSON(`[{"namespace":`); err == nil {
			t.Fatal("expected an error for malformed JSON")
		}
	})

	t.Run("json object instead of array is rejected", func(t *testing.T) {
		if _, err := parseNamespaceRuleJSON(`{"namespace":"monitoring"}`); err == nil {
			t.Fatal("expected an error for a JSON object")
		}
	})

	t.Run("rule without namespace is rejected", func(t *testing.T) {
		if _, err := parseNamespaceRuleJSON(`[{"ports":[4318]}]`); err == nil {
			t.Fatal("expected an error for a missing namespace")
		}
	})
}
