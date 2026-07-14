//go:build e2e

package e2e

import "k8s.io/apimachinery/pkg/api/resource"

func mustParseQuantity(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}
