package cloud

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

var _ Provider = (*AWS)(nil)

func apiErr(code string) error {
	// Wrapped like the SDK wraps operation errors.
	return fmt.Errorf("operation error EC2: RunInstances: %w", &smithy.GenericAPIError{Code: code, Message: "msg for " + code})
}

func TestAWSErrorMapping(t *testing.T) {
	cases := []struct {
		code string
		want error
	}{
		{"InsufficientInstanceCapacity", ErrUnavailable},
		{"InsufficientCapacity", ErrUnavailable},
		{"Unsupported", ErrUnavailable},
		{"SpotMaxPriceTooLow", ErrUnavailable},
		{"InstanceLimitExceeded", ErrQuota},
		{"VcpuLimitExceeded", ErrQuota},
		{"MaxSpotInstanceCountExceeded", ErrQuota},
		{"RequestLimitExceeded", ErrQuota},
		{"InvalidParameterValue", nil},
	}
	for _, c := range cases {
		err := awsError("run instance", apiErr(c.code))
		if err == nil {
			t.Fatalf("%s: nil error", c.code)
		}
		for _, sentinel := range []error{ErrUnavailable, ErrQuota} {
			if got := errors.Is(err, sentinel); got != (sentinel == c.want) {
				t.Errorf("%s: errors.Is(%v) = %v", c.code, sentinel, got)
			}
		}
		if want := "msg for " + c.code; !strings.Contains(err.Error(), want) {
			t.Errorf("%s: message %q lacks %q", c.code, err, want)
		}
	}
	if awsError("x", nil) != nil {
		t.Error("nil error must stay nil")
	}
	if err := awsError("x", context.DeadlineExceeded); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("non-API error lost: %v", err)
	}
}

func TestAWSSpotCapacityError(t *testing.T) {
	for _, code := range []string{"InsufficientInstanceCapacity", "SpotMaxPriceTooLow", "MaxSpotInstanceCountExceeded"} {
		if !awsSpotCapacityError(apiErr(code)) {
			t.Errorf("%s should allow on-demand fallback", code)
		}
	}
	for _, code := range []string{"VcpuLimitExceeded", "InvalidAMIID.NotFound"} {
		if awsSpotCapacityError(apiErr(code)) {
			t.Errorf("%s should not allow on-demand fallback", code)
		}
	}
}

func TestAWSStateMapping(t *testing.T) {
	cases := map[types.InstanceStateName]string{
		types.InstanceStateNamePending:      StatePending,
		types.InstanceStateNameRunning:      StateRunning,
		types.InstanceStateNameStopping:     StateStopped,
		types.InstanceStateNameStopped:      StateStopped,
		types.InstanceStateNameShuttingDown: StateTerminated,
		types.InstanceStateNameTerminated:   StateTerminated,
	}
	for in, want := range cases {
		if got := awsState(in); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
}

func TestAWSInstance(t *testing.T) {
	launched := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	raw := types.Instance{
		InstanceId:        aws.String("i-123"),
		InstanceType:      types.InstanceType("c7a.4xlarge"),
		Placement:         &types.Placement{AvailabilityZone: aws.String("eu-central-1a")},
		PublicIpAddress:   aws.String("203.0.113.9"),
		State:             &types.InstanceState{Name: types.InstanceStateNameTerminated},
		StateReason:       &types.StateReason{Code: aws.String("Server.SpotInstanceTermination")},
		InstanceLifecycle: types.InstanceLifecycleTypeSpot,
		LaunchTime:        &launched,
		Tags: []types.Tag{
			{Key: aws.String("Name"), Value: aws.String("worker-1")},
			{Key: aws.String(LabelManaged), Value: aws.String("true")},
		},
	}
	in := awsInstance(&raw)
	if in.ID != "i-123" || in.Name != "worker-1" || in.Type != "c7a.4xlarge" || in.Location != "eu-central-1a" ||
		in.PublicIP != "203.0.113.9" || !in.Spot || !in.CreatedAt.Equal(launched) || in.Labels[LabelManaged] != "true" {
		t.Fatalf("unexpected instance %+v", in)
	}
	if !in.SpotInterrupted() {
		t.Error("expected spot interruption")
	}

	raw.StateReason = &types.StateReason{Code: aws.String("Client.InstanceInitiatedShutdown")}
	if in := awsInstance(&raw); in.SpotInterrupted() {
		t.Error("self shutdown is not an interruption")
	}
	raw.InstanceLifecycle = ""
	raw.StateReason = &types.StateReason{Code: aws.String("Server.SpotInstanceTermination")}
	if in := awsInstance(&raw); in.Spot || in.SpotInterrupted() {
		t.Error("on-demand instance can't be spot-interrupted")
	}
}

func TestAWSImage(t *testing.T) {
	img := awsImage(&types.Image{
		ImageId:      aws.String("ami-1"),
		Name:         aws.String("chiperka-worker-1"),
		CreationDate: aws.String("2026-03-04T05:06:07.000Z"),
		BlockDeviceMappings: []types.BlockDeviceMapping{
			{Ebs: &types.EbsBlockDevice{VolumeSize: aws.Int32(60)}},
			{Ebs: &types.EbsBlockDevice{VolumeSize: aws.Int32(8)}},
			{VirtualName: aws.String("ephemeral0")},
		},
	})
	if img.ID != "ami-1" || img.SizeGB != 68 || img.CreatedAt.IsZero() {
		t.Fatalf("unexpected image %+v", img)
	}
}

func TestAWSDefaultsAndPrices(t *testing.T) {
	a := NewAWS("", "", "")
	if a.Region != "eu-central-1" || a.Name() != "aws" || a.Currency() != "USD" {
		t.Fatalf("bad defaults %+v", a)
	}
	p, err := a.HourlyPrice(context.Background(), "c7a.4xlarge", "eu-central-1a", false)
	if err != nil || p < 0.9 || p > 1.0 {
		t.Fatalf("c7a.4xlarge price %v, %v", p, err)
	}
	if _, err := a.HourlyPrice(context.Background(), "x1.nope", "eu-central-1a", false); err == nil {
		t.Error("unknown type must fail")
	}
}
