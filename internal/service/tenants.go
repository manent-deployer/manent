package service

import (
	"context"
)

// TenantService exposes the tenant registry: the teams and the applications they own.
type TenantService interface {
	// Tenants lists the tenants, optionally the owner of an application and matching a
	// tenant selector.
	Tenants(ctx context.Context, f TenantFilter) ([]TenantInfo, error)
	// Tenant returns a tenant.
	Tenant(ctx context.Context, name string) (*TenantInfo, error)
}

// TenantFilter filters the tenants.
type TenantFilter struct {
	// Application keeps only its owner.
	Application string `json:"application,omitempty"`
	// Selector matches the tenant tags and "name".
	Selector string `json:"selector,omitempty"`
}

// TenantApplication is an application owned by a tenant.
type TenantApplication struct {
	Tags map[string]string `json:"tags,omitempty"`
}

// TenantInfo is a tenant as exposed by the API.
type TenantInfo struct {
	Name         string                       `json:"name"`
	Description  string                       `json:"description,omitempty"`
	Tags         map[string]string            `json:"tags,omitempty"`
	Applications map[string]TenantApplication `json:"applications,omitempty"`
	// Queue is the tenant's deploy queue; empty: the default queue.
	Queue       string `json:"queue,omitempty"`
	Concurrency int    `json:"concurrency,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
	Source      string `json:"source,omitempty"`
	Revision    string `json:"revision,omitempty"`
}
