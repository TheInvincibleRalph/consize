package costscan

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
)

type mockEC2 struct {
	volumes   []ec2types.Volume
	instances []ec2types.Instance
}

func (m *mockEC2) DescribeVolumes(ctx context.Context, params *ec2.DescribeVolumesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	return &ec2.DescribeVolumesOutput{Volumes: m.volumes}, nil
}
func (m *mockEC2) DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{
		Reservations: []ec2types.Reservation{{Instances: m.instances}},
	}, nil
}
func (m *mockEC2) DeleteVolume(ctx context.Context, params *ec2.DeleteVolumeInput, optFns ...func(*ec2.Options)) (*ec2.DeleteVolumeOutput, error) {
	return &ec2.DeleteVolumeOutput{}, nil
}
func (m *mockEC2) TerminateInstances(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	return &ec2.TerminateInstancesOutput{}, nil
}

type mockELB struct{}

func (m *mockELB) DescribeLoadBalancers(ctx context.Context, params *elasticloadbalancingv2.DescribeLoadBalancersInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeLoadBalancersOutput, error) {
	return &elasticloadbalancingv2.DescribeLoadBalancersOutput{}, nil
}
func (m *mockELB) DescribeTargetGroups(ctx context.Context, params *elasticloadbalancingv2.DescribeTargetGroupsInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeTargetGroupsOutput, error) {
	return &elasticloadbalancingv2.DescribeTargetGroupsOutput{}, nil
}
func (m *mockELB) DescribeTargetHealth(ctx context.Context, params *elasticloadbalancingv2.DescribeTargetHealthInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DescribeTargetHealthOutput, error) {
	return &elasticloadbalancingv2.DescribeTargetHealthOutput{}, nil
}
func (m *mockELB) DeleteLoadBalancer(ctx context.Context, params *elasticloadbalancingv2.DeleteLoadBalancerInput, optFns ...func(*elasticloadbalancingv2.Options)) (*elasticloadbalancingv2.DeleteLoadBalancerOutput, error) {
	return &elasticloadbalancingv2.DeleteLoadBalancerOutput{}, nil
}

func ptr[T any](v T) *T { return &v }

func TestAWSSource_Scan(t *testing.T) {
	ec2Mock := &mockEC2{
		volumes: []ec2types.Volume{
			{
				VolumeId:    ptr("vol-123"),
				State:       ec2types.VolumeStateAvailable,
				Size:        ptr(int32(100)),
				Attachments: []ec2types.VolumeAttachment{}, // Unattached
			},
			{
				VolumeId:    ptr("vol-456"),
				State:       ec2types.VolumeStateInUse,
				Size:        ptr(int32(50)),
				Attachments: []ec2types.VolumeAttachment{{}}, // Attached
			},
		},
		instances: []ec2types.Instance{
			{
				InstanceId: ptr("i-abc"),
				State:      &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
			},
			{
				InstanceId: ptr("i-def"),
				State:      &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
			},
		},
	}

	src := &AWSSource{
		ec2Client: ec2Mock,
		elbClient: &mockELB{},
		region:    "us-east-1",
		accountID: "test-account",
	}

	opps, err := src.Scan(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(opps) != 2 {
		t.Fatalf("expected 2 opportunities, got %d", len(opps))
	}

	if opps[0].ResourceID != "vol-123" {
		t.Errorf("expected vol-123, got %s", opps[0].ResourceID)
	}
	if opps[1].ResourceID != "i-abc" {
		t.Errorf("expected i-abc, got %s", opps[1].ResourceID)
	}
}
