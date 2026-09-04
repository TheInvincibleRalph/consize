package costscan

import (
	"context"
	"fmt"
	"strings"
	"time"

	"consize/internal/store"
)

const (
	TypeUnattachedVolume = "unattached_volume"
	TypeIdleLoadBalancer = "idle_load_balancer"
	TypeUnusedNATGateway = "unused_nat_gateway"
	TypeStoppedInstance  = "stopped_instance"
)

type Source interface {
	Scan(ctx context.Context) ([]store.CostOpportunity, error)
}

type DirectApplier interface {
	ApplyDirect(ctx context.Context, opp store.CostOpportunity, mode string) (DirectApplyResult, error)
}

type DirectApplyResult struct {
	OpportunityID int64
	Provider      string
	ResourceType  string
	ResourceID    string
	Name          string
	Mode          string
	Applied       bool
	Message       string
}

type Service struct {
	Source Source
	Store  store.Store
}

func (s Service) Run(ctx context.Context) ([]store.CostOpportunity, error) {
	if s.Source == nil {
		return nil, fmt.Errorf("cloud waste scanner is not configured; set CONSIZE_COSTSCAN=gcp or run the fixture demo explicitly")
	}
	opps, err := s.Source.Scan(ctx)
	if err != nil {
		return nil, err
	}
	if s.Store != nil {
		if err := s.Store.UpsertCostOpportunities(ctx, opps); err != nil {
			return nil, err
		}
		return s.Store.ListCostOpportunities(ctx, store.OpportunityOpen)
	}
	return opps, nil
}

func SourceFor(kind string) (Source, string, error) {
	switch strings.TrimSpace(strings.ToLower(kind)) {
	case "", "none", "disabled":
		return nil, "disabled", nil
	case "fixture":
		return FixtureSource{}, "fixture", nil
	case "gcp":
		return NewGCPSource(), "Google Compute Engine (CONSIZE_COSTSCAN=gcp)", nil
	}
	return nil, "", fmt.Errorf("unknown source %q (supported: none, fixture, gcp)", kind)
}

func DirectApplierFor(provider string) (DirectApplier, error) {
	switch strings.TrimSpace(strings.ToLower(provider)) {
	case "gcp":
		return NewGCPSource(), nil
	case "aws", "":
		return nil, fmt.Errorf("direct cleanup is not configured for provider %q yet; open an IaC PR instead", provider)
	default:
		return nil, fmt.Errorf("direct cleanup is not supported for provider %q", provider)
	}
}

type FixtureSource struct{}

func (FixtureSource) Scan(context.Context) ([]store.CostOpportunity, error) {
	now := time.Now().UTC()
	return []store.CostOpportunity{
		{
			Provider:       "aws",
			Account:        "demo-prod",
			Region:         "us-east-1",
			ResourceType:   TypeUnattachedVolume,
			ResourceID:     "vol-0f3a9c1demo",
			Name:           "checkout-old-data",
			MonthlyCost:    12.40,
			Recommendation: "Delete unattached EBS volume after snapshot confirmation.",
			Action:         "delete_volume",
			Risk:           "medium",
			Status:         store.OpportunityOpen,
			Evidence: map[string]any{
				"size_gib":       124,
				"volume_type":    "gp3",
				"attached":       false,
				"unattached_for": "11d",
			},
			IaCRepo:       "platform/infra-live",
			IaCPath:       "aws/prod/storage.tf",
			TerraformAddr: `aws_ebs_volume.checkout_old_data`,
			FirstSeenAt:   now.Add(-11 * 24 * time.Hour),
			LastSeenAt:    now,
		},
		{
			Provider:       "aws",
			Account:        "demo-prod",
			Region:         "us-east-1",
			ResourceType:   TypeIdleLoadBalancer,
			ResourceID:     "arn:aws:elasticloadbalancing:us-east-1:111122223333:loadbalancer/app/legacy-api/50dc6c495c0c9188",
			Name:           "legacy-api",
			MonthlyCost:    18.25,
			Recommendation: "Remove idle application load balancer with no healthy targets.",
			Action:         "remove_load_balancer",
			Risk:           "high",
			Status:         store.OpportunityOpen,
			Evidence: map[string]any{
				"healthy_targets":  0,
				"request_count_7d": 0,
				"scheme":           "internet-facing",
			},
			IaCRepo:       "platform/infra-live",
			IaCPath:       "aws/prod/load-balancers.tf",
			TerraformAddr: `aws_lb.legacy_api`,
			FirstSeenAt:   now.Add(-8 * 24 * time.Hour),
			LastSeenAt:    now,
		},
		{
			Provider:       "aws",
			Account:        "demo-prod",
			Region:         "us-east-1",
			ResourceType:   TypeUnusedNATGateway,
			ResourceID:     "nat-0123demo456",
			Name:           "sandbox-public-a",
			MonthlyCost:    32.85,
			Recommendation: "Remove unused NAT gateway with no routed private subnet traffic.",
			Action:         "remove_nat_gateway",
			Risk:           "medium",
			Status:         store.OpportunityOpen,
			Evidence: map[string]any{
				"bytes_out_7d": 0,
				"packets_7d":   0,
				"route_tables": 0,
			},
			IaCRepo:       "platform/infra-live",
			IaCPath:       "aws/sandbox/network.tf",
			TerraformAddr: `aws_nat_gateway.sandbox_public_a`,
			FirstSeenAt:   now.Add(-14 * 24 * time.Hour),
			LastSeenAt:    now,
		},
		{
			Provider:       "aws",
			Account:        "demo-prod",
			Region:         "us-east-1",
			ResourceType:   TypeStoppedInstance,
			ResourceID:     "i-0abc123demo",
			Name:           "qa-runner-legacy",
			MonthlyCost:    9.80,
			Recommendation: "Terminate stopped instance after owner approval; attached storage is still billed.",
			Action:         "terminate_instance",
			Risk:           "low",
			Status:         store.OpportunityOpen,
			Evidence: map[string]any{
				"state":           "stopped",
				"stopped_for":     "19d",
				"root_volume_gib": 80,
			},
			IaCRepo:       "platform/infra-live",
			IaCPath:       "aws/nonprod/compute.tf",
			TerraformAddr: `aws_instance.qa_runner_legacy`,
			FirstSeenAt:   now.Add(-19 * 24 * time.Hour),
			LastSeenAt:    now,
		},
	}, nil
}
