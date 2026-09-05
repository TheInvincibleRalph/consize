package costscan

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"consize/internal/store"
)

type EC2API interface {
	DescribeVolumes(ctx context.Context, params *ec2.DescribeVolumesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error)
	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DeleteVolume(ctx context.Context, params *ec2.DeleteVolumeInput, optFns ...func(*ec2.Options)) (*ec2.DeleteVolumeOutput, error)
	TerminateInstances(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
}

type ELBAPI interface {
	DescribeLoadBalancers(ctx context.Context, params *elasticloadbalancingv2.DescribeLoadBalancersInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeLoadBalancersOutput, error)
	DescribeTargetGroups(ctx context.Context, params *elasticloadbalancingv2.DescribeTargetGroupsInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeTargetGroupsOutput, error)
	DescribeTargetHealth(ctx context.Context, params *elasticloadbalancingv2.DescribeTargetHealthInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeTargetHealthOutput, error)
	DeleteLoadBalancer(ctx context.Context, params *elasticloadbalancingv2.DeleteLoadBalancerInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DeleteLoadBalancerOutput, error)
}

type AWSSource struct {
	ec2Client EC2API
	elbClient ELBAPI
	region    string
	accountID string
}

func NewAWSSource(ctx context.Context) (*AWSSource, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	return &AWSSource{
		ec2Client: ec2.NewFromConfig(cfg),
		elbClient: elasticloadbalancingv2.NewFromConfig(cfg),
		region:    cfg.Region,
		accountID: "aws-account", // STSOperations could fetch this if needed
	}, nil
}

func (s *AWSSource) Scan(ctx context.Context) ([]store.CostOpportunity, error) {
	var opps []store.CostOpportunity

	volumes, err := s.scanUnattachedVolumes(ctx)
	if err != nil {
		return nil, err
	}
	opps = append(opps, volumes...)

	instances, err := s.scanStoppedInstances(ctx)
	if err != nil {
		return nil, err
	}
	opps = append(opps, instances...)

	return opps, nil
}

func (s *AWSSource) scanUnattachedVolumes(ctx context.Context) ([]store.CostOpportunity, error) {
	var opps []store.CostOpportunity
	now := time.Now().UTC()

	paginator := ec2.NewDescribeVolumesPaginator(s.ec2Client, &ec2.DescribeVolumesInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("describing volumes: %w", err)
		}

		for _, vol := range page.Volumes {
			if len(vol.Attachments) == 0 && vol.State == ec2types.VolumeStateAvailable {
				name := *vol.VolumeId
				for _, tag := range vol.Tags {
					if *tag.Key == "Name" {
						name = *tag.Value
						break
					}
				}

				opps = append(opps, store.CostOpportunity{
					Provider:       "aws",
					Account:        s.accountID,
					Region:         s.region,
					ResourceType:   TypeUnattachedVolume,
					ResourceID:     *vol.VolumeId,
					Name:           name,
					MonthlyCost:    float64(*vol.Size) * 0.08, // approximation for gp3
					Recommendation: "Delete unattached EBS volume.",
					Action:         "delete_volume",
					Risk:           "medium",
					Status:         store.OpportunityOpen,
					Evidence: map[string]any{
						"size_gib":    *vol.Size,
						"volume_type": string(vol.VolumeType),
						"attached":    false,
					},
					FirstSeenAt: now,
					LastSeenAt:  now,
				})
			}
		}
	}
	return opps, nil
}

func (s *AWSSource) scanStoppedInstances(ctx context.Context) ([]store.CostOpportunity, error) {
	var opps []store.CostOpportunity
	now := time.Now().UTC()

	paginator := ec2.NewDescribeInstancesPaginator(s.ec2Client, &ec2.DescribeInstancesInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("describing instances: %w", err)
		}

		for _, res := range page.Reservations {
			for _, inst := range res.Instances {
				if inst.State.Name == ec2types.InstanceStateNameStopped {
					name := *inst.InstanceId
					for _, tag := range inst.Tags {
						if *tag.Key == "Name" {
							name = *tag.Value
							break
						}
					}

					opps = append(opps, store.CostOpportunity{
						Provider:       "aws",
						Account:        s.accountID,
						Region:         s.region,
						ResourceType:   TypeStoppedInstance,
						ResourceID:     *inst.InstanceId,
						Name:           name,
						MonthlyCost:    10.0, // stub cost
						Recommendation: "Terminate stopped instance; attached storage is still billed.",
						Action:         "terminate_instance",
						Risk:           "low",
						Status:         store.OpportunityOpen,
						Evidence: map[string]any{
							"state":         "stopped",
							"instance_type": string(inst.InstanceType),
						},
						FirstSeenAt: now,
						LastSeenAt:  now,
					})
				}
			}
		}
	}
	return opps, nil
}

func (s *AWSSource) ApplyDirect(ctx context.Context, opp store.CostOpportunity, mode string) (DirectApplyResult, error) {
	res := DirectApplyResult{
		OpportunityID: opp.ID,
		Provider:      "aws",
		ResourceType:  opp.ResourceType,
		ResourceID:    opp.ResourceID,
		Name:          opp.Name,
		Mode:          mode,
	}

	switch opp.Action {
	case "delete_volume":
		if mode == "apply" {
			_, err := s.ec2Client.DeleteVolume(ctx, &ec2.DeleteVolumeInput{
				VolumeId: &opp.ResourceID,
			})
			if err != nil {
				return res, fmt.Errorf("deleting volume: %w", err)
			}
			res.Applied = true
			res.Message = "Volume deleted successfully."
		} else {
			res.Message = "Dry run: would delete volume."
		}
		return res, nil
	case "terminate_instance":
		if mode == "apply" {
			_, err := s.ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
				InstanceIds: []string{opp.ResourceID},
			})
			if err != nil {
				return res, fmt.Errorf("terminating instance: %w", err)
			}
			res.Applied = true
			res.Message = "Instance terminated successfully."
		} else {
			res.Message = "Dry run: would terminate instance."
		}
		return res, nil
	}
	return res, fmt.Errorf("unsupported action %q", opp.Action)
}
