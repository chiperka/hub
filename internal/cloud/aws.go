package cloud

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

// AWS talks to EC2. The credentials should be limited by the policy in
// contrib/cloud-image/aws-iam-policy.json.
type AWS struct {
	Region          string
	AccessKeyID     string
	SecretAccessKey string

	mu         sync.Mutex
	client     *ec2.Client
	subnets    map[string]awsSubnet // AZ -> default subnet
	rootDevice map[string]string    // AMI -> root device name
	baseImage  string
	baseAt     time.Time
	spotPrices map[string]awsSpotPrice
}

type awsSubnet struct{ ID, VpcID string }

type awsSpotPrice struct {
	Price float64
	At    time.Time
}

// NewAWS uses the given keys, or the SDK default credential chain when they are empty.
func NewAWS(region, accessKeyID, secretAccessKey string) *AWS {
	if region == "" {
		region = "eu-central-1"
	}
	return &AWS{Region: region, AccessKeyID: accessKeyID, SecretAccessKey: secretAccessKey}
}

func (a *AWS) Name() string     { return "aws" }
func (a *AWS) Currency() string { return "USD" }

const (
	awsResourceName = "chiperka-autoscaler"
	awsUbuntuOwner  = "099720109477" // Canonical
	awsUbuntuName   = "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"
	awsSpotCacheTTL = 10 * time.Minute
)

// ec2 builds the client on first use so a missing config doesn't fail at startup.
func (a *AWS) ec2(ctx context.Context) (*ec2.Client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != nil {
		return a.client, nil
	}
	opts := []func(*config.LoadOptions) error{config.WithRegion(a.Region)}
	if a.AccessKeyID != "" && a.SecretAccessKey != "" {
		opts = append(opts, config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(a.AccessKeyID, a.SecretAccessKey, "")))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("aws config: %v: %w", err, ErrNotConfigured)
	}
	if cfg.Credentials == nil {
		return nil, ErrNotConfigured
	}
	if _, err := cfg.Credentials.Retrieve(ctx); err != nil {
		return nil, fmt.Errorf("aws credentials: %v: %w", err, ErrNotConfigured)
	}
	a.client = ec2.NewFromConfig(cfg)
	return a.client, nil
}

// ── errors ──────────────────────────────────────────────────────────────────

func awsCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}

// Spot launch failures that an on-demand launch may not hit.
func awsSpotCapacityError(err error) bool {
	switch awsCode(err) {
	case "InsufficientInstanceCapacity", "InsufficientCapacity", "InsufficientSpotInstanceCapacity",
		"SpotMaxPriceTooLow", "MaxSpotInstanceCountExceeded":
		return true
	}
	return false
}

// awsError maps EC2 error codes onto ErrUnavailable / ErrQuota and keeps the AWS message.
func awsError(op string, err error) error {
	if err == nil {
		return nil
	}
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return fmt.Errorf("aws %s: %w", op, err)
	}
	var kind error
	switch ae.ErrorCode() {
	case "InsufficientInstanceCapacity", "InsufficientCapacity", "InsufficientSpotInstanceCapacity",
		"InsufficientHostCapacity", "Unsupported", "SpotMaxPriceTooLow":
		kind = ErrUnavailable
	case "InstanceLimitExceeded", "VcpuLimitExceeded", "MaxSpotInstanceCountExceeded", "RequestLimitExceeded":
		kind = ErrQuota
	default:
		return fmt.Errorf("aws %s: %w", op, err)
	}
	return fmt.Errorf("aws %s: %s (%s): %w", op, ae.ErrorMessage(), ae.ErrorCode(), kind)
}

func tagFilter(key, value string) types.Filter {
	return types.Filter{Name: aws.String("tag:" + key), Values: []string{value}}
}

func filter(name string, values ...string) types.Filter {
	return types.Filter{Name: aws.String(name), Values: values}
}

func awsTags(labels map[string]string) []types.Tag {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tags := make([]types.Tag, 0, len(keys))
	for _, k := range keys {
		tags = append(tags, types.Tag{Key: aws.String(k), Value: aws.String(labels[k])})
	}
	return tags
}

func awsLabels(tags []types.Tag) map[string]string {
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

func (a *AWS) Check(ctx context.Context) error {
	c, err := a.ec2(ctx)
	if err != nil {
		return err
	}
	_, err = c.DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{})
	return awsError("check", err)
}

// ── shared resources ────────────────────────────────────────────────────────

func (a *AWS) defaultVPC(ctx context.Context, c *ec2.Client) (string, error) {
	res, err := c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{Filters: []types.Filter{filter("is-default", "true")}})
	if err != nil {
		return "", awsError("describe VPCs", err)
	}
	if len(res.Vpcs) == 0 {
		return "", fmt.Errorf("aws: region %s has no default VPC", a.Region)
	}
	return aws.ToString(res.Vpcs[0].VpcId), nil
}

// sgName is per hub: hubs (and the image builder) sharing an account must not
// overwrite each other's SSH rule.
func sgName(hubID string) string { return awsResourceName + "-" + hubID }

func (a *AWS) securityGroup(ctx context.Context, c *ec2.Client, vpcID, hubID string) (*types.SecurityGroup, error) {
	res, err := c.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{Filters: []types.Filter{
		filter("group-name", sgName(hubID)), filter("vpc-id", vpcID),
	}})
	if err != nil {
		return nil, awsError("describe security groups", err)
	}
	if len(res.SecurityGroups) == 0 {
		return nil, nil
	}
	return &res.SecurityGroups[0], nil
}

func (a *AWS) Prepare(ctx context.Context, p PrepareRequest) error {
	if p.SSHAllowCIDR == "" {
		return fmt.Errorf("the IP address SSH may come from is not set")
	}
	c, err := a.ec2(ctx)
	if err != nil {
		return err
	}
	vpcID, err := a.defaultVPC(ctx, c)
	if err != nil {
		return err
	}
	sg, err := a.securityGroup(ctx, c, vpcID, p.HubID)
	if err != nil {
		return err
	}
	var groupID string
	if sg == nil {
		res, err := c.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
			GroupName:   aws.String(sgName(p.HubID)),
			Description: aws.String("Chiperka autoscaler workers: SSH from the hub only"),
			VpcId:       aws.String(vpcID),
			TagSpecifications: []types.TagSpecification{{
				ResourceType: types.ResourceTypeSecurityGroup,
				Tags:         awsTags(map[string]string{LabelManaged: "true", "Name": sgName(p.HubID)}),
			}},
		})
		if err != nil {
			return awsError("create security group", err)
		}
		groupID = aws.ToString(res.GroupId)
	} else {
		groupID = aws.ToString(sg.GroupId)
		if awsLabels(sg.Tags)[LabelManaged] != "true" {
			_, err := c.CreateTags(ctx, &ec2.CreateTagsInput{Resources: []string{groupID}, Tags: awsTags(map[string]string{LabelManaged: "true"})})
			if err != nil {
				return awsError("tag security group", err)
			}
		}
	}

	// Keep exactly one ingress rule: TCP 22 from the hub. Egress stays as created.
	ipv6 := strings.Contains(p.SSHAllowCIDR, ":")
	var revoke []string
	found := false
	pg := ec2.NewDescribeSecurityGroupRulesPaginator(c, &ec2.DescribeSecurityGroupRulesInput{Filters: []types.Filter{filter("group-id", groupID)}})
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			return awsError("describe security group rules", err)
		}
		for _, r := range page.SecurityGroupRules {
			if aws.ToBool(r.IsEgress) {
				continue
			}
			cidr := aws.ToString(r.CidrIpv4)
			if ipv6 {
				cidr = aws.ToString(r.CidrIpv6)
			}
			if !found && aws.ToString(r.IpProtocol) == "tcp" && aws.ToInt32(r.FromPort) == 22 && aws.ToInt32(r.ToPort) == 22 &&
				cidr == p.SSHAllowCIDR && r.PrefixListId == nil && r.ReferencedGroupInfo == nil {
				found = true
				continue
			}
			revoke = append(revoke, aws.ToString(r.SecurityGroupRuleId))
		}
	}
	if len(revoke) > 0 {
		_, err := c.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{GroupId: aws.String(groupID), SecurityGroupRuleIds: revoke})
		if err != nil {
			return awsError("revoke security group rules", err)
		}
	}
	if !found {
		perm := types.IpPermission{IpProtocol: aws.String("tcp"), FromPort: aws.Int32(22), ToPort: aws.Int32(22)}
		if ipv6 {
			perm.Ipv6Ranges = []types.Ipv6Range{{CidrIpv6: aws.String(p.SSHAllowCIDR), Description: aws.String("Chiperka hub")}}
		} else {
			perm.IpRanges = []types.IpRange{{CidrIp: aws.String(p.SSHAllowCIDR), Description: aws.String("Chiperka hub")}}
		}
		_, err := c.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{GroupId: aws.String(groupID), IpPermissions: []types.IpPermission{perm}})
		if err != nil {
			return awsError("authorize SSH", err)
		}
	}
	return nil
}

// ── servers ─────────────────────────────────────────────────────────────────

func awsState(s types.InstanceStateName) string {
	switch s {
	case types.InstanceStateNameRunning:
		return StateRunning
	case types.InstanceStateNameStopping, types.InstanceStateNameStopped:
		return StateStopped
	case types.InstanceStateNameShuttingDown, types.InstanceStateNameTerminated:
		return StateTerminated
	default: // pending
		return StatePending
	}
}

func awsInstance(i *types.Instance) Instance {
	labels := awsLabels(i.Tags)
	in := Instance{
		ID:       aws.ToString(i.InstanceId),
		Name:     labels["Name"],
		Type:     string(i.InstanceType),
		PublicIP: aws.ToString(i.PublicIpAddress),
		Spot:     i.InstanceLifecycle == types.InstanceLifecycleTypeSpot,
		Labels:   labels,
	}
	if i.Placement != nil {
		in.Location = aws.ToString(i.Placement.AvailabilityZone)
	}
	if i.State != nil {
		in.State = awsState(i.State.Name)
	} else {
		in.State = StatePending
	}
	if i.StateReason != nil {
		in.StateReason = aws.ToString(i.StateReason.Code)
	}
	if i.LaunchTime != nil {
		in.CreatedAt = *i.LaunchTime
	}
	return in
}

// subnet returns the default-VPC default subnet of an availability zone.
func (a *AWS) subnet(ctx context.Context, c *ec2.Client, az string) (awsSubnet, error) {
	a.mu.Lock()
	s, ok := a.subnets[az]
	a.mu.Unlock()
	if ok {
		return s, nil
	}
	res, err := c.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{Filters: []types.Filter{
		filter("availability-zone", az), filter("default-for-az", "true"),
	}})
	if err != nil {
		return s, awsError("describe subnets", err)
	}
	if len(res.Subnets) == 0 {
		return s, fmt.Errorf("aws: no default subnet in availability zone %q", az)
	}
	s = awsSubnet{ID: aws.ToString(res.Subnets[0].SubnetId), VpcID: aws.ToString(res.Subnets[0].VpcId)}
	a.mu.Lock()
	if a.subnets == nil {
		a.subnets = map[string]awsSubnet{}
	}
	a.subnets[az] = s
	a.mu.Unlock()
	return s, nil
}

func (a *AWS) imageRootDevice(ctx context.Context, c *ec2.Client, image string) (string, error) {
	a.mu.Lock()
	dev, ok := a.rootDevice[image]
	a.mu.Unlock()
	if ok {
		return dev, nil
	}
	res, err := c.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{image}})
	if err != nil {
		return "", awsError("describe image", err)
	}
	if len(res.Images) == 0 {
		return "", fmt.Errorf("aws: image %s not found", image)
	}
	dev = aws.ToString(res.Images[0].RootDeviceName)
	if dev == "" {
		dev = "/dev/sda1"
	}
	a.mu.Lock()
	if a.rootDevice == nil {
		a.rootDevice = map[string]string{}
	}
	a.rootDevice[image] = dev
	a.mu.Unlock()
	return dev, nil
}

func (a *AWS) Create(ctx context.Context, r CreateRequest) (*Instance, error) {
	c, err := a.ec2(ctx)
	if err != nil {
		return nil, err
	}
	image := r.Image
	if image == "" {
		if img, err := a.LatestImage(ctx); err == nil && img != nil {
			image = img.ID
		} else if image, err = a.BaseImage(ctx); err != nil {
			return nil, err
		}
	}
	subnet, err := a.subnet(ctx, c, r.Location)
	if err != nil {
		return nil, err
	}
	sg, err := a.securityGroup(ctx, c, subnet.VpcID, r.Labels[LabelHub])
	if err != nil {
		return nil, err
	}
	if sg == nil {
		return nil, fmt.Errorf("aws: security group %s is missing, run Prepare first", sgName(r.Labels[LabelHub]))
	}
	rootDev, err := a.imageRootDevice(ctx, c, image)
	if err != nil {
		return nil, err
	}
	disk := r.DiskGB
	if disk <= 0 {
		disk = 60
	}
	labels := make(map[string]string, len(r.Labels)+1)
	for k, v := range r.Labels {
		labels[k] = v
	}
	labels["Name"] = r.Name
	tags := awsTags(labels)

	run := func(spot bool) (*ec2.RunInstancesOutput, error) {
		in := &ec2.RunInstancesInput{
			ImageId:          aws.String(image),
			InstanceType:     types.InstanceType(r.Type),
			MinCount:         aws.Int32(1),
			MaxCount:         aws.Int32(1),
			SubnetId:         aws.String(subnet.ID),
			SecurityGroupIds: []string{aws.ToString(sg.GroupId)},
			UserData:         aws.String(base64.StdEncoding.EncodeToString([]byte(r.UserData))),
			BlockDeviceMappings: []types.BlockDeviceMapping{{
				DeviceName: aws.String(rootDev),
				Ebs: &types.EbsBlockDevice{
					VolumeSize:          aws.Int32(int32(disk)),
					VolumeType:          types.VolumeTypeGp3,
					DeleteOnTermination: aws.Bool(true),
				},
			}},
			// The worker removes itself with `shutdown`.
			InstanceInitiatedShutdownBehavior: shutdownBehavior(r.StopOnShutdown),
			// IMDSv2 only, one hop: containers on the server can't reach it.
			MetadataOptions: &types.InstanceMetadataOptionsRequest{
				HttpEndpoint:            types.InstanceMetadataEndpointStateEnabled,
				HttpTokens:              types.HttpTokensStateRequired,
				HttpPutResponseHopLimit: aws.Int32(1),
			},
			TagSpecifications: []types.TagSpecification{
				{ResourceType: types.ResourceTypeInstance, Tags: tags},
				{ResourceType: types.ResourceTypeVolume, Tags: tags},
			},
		}
		if spot {
			in.InstanceMarketOptions = &types.InstanceMarketOptionsRequest{
				MarketType: types.MarketTypeSpot,
				SpotOptions: &types.SpotMarketOptions{
					SpotInstanceType:             types.SpotInstanceTypeOneTime,
					InstanceInterruptionBehavior: types.InstanceInterruptionBehaviorTerminate,
				},
			}
		}
		return c.RunInstances(ctx, in)
	}

	res, err := run(r.Spot)
	if err != nil && r.Spot && r.SpotFallback && awsSpotCapacityError(err) {
		res, err = run(false)
	}
	if err != nil {
		return nil, awsError("run instance", err)
	}
	if len(res.Instances) == 0 {
		return nil, fmt.Errorf("aws: RunInstances returned no instance")
	}
	in := awsInstance(&res.Instances[0])
	return &in, nil
}

func (a *AWS) Delete(ctx context.Context, id string) error {
	c, err := a.ec2(ctx)
	if err != nil {
		return err
	}
	_, err = c.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}})
	if awsCode(err) == "InvalidInstanceID.NotFound" {
		return nil
	}
	return awsError("terminate instance", err)
}

// List includes terminated instances: they linger for about an hour, which
// lets the caller notice spot interruptions.
func (a *AWS) List(ctx context.Context) ([]Instance, error) {
	c, err := a.ec2(ctx)
	if err != nil {
		return nil, err
	}
	var out []Instance
	pg := ec2.NewDescribeInstancesPaginator(c, &ec2.DescribeInstancesInput{Filters: []types.Filter{tagFilter(LabelManaged, "true")}})
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			return nil, awsError("describe instances", err)
		}
		for _, res := range page.Reservations {
			for i := range res.Instances {
				out = append(out, awsInstance(&res.Instances[i]))
			}
		}
	}
	return out, nil
}

// ── prices ──────────────────────────────────────────────────────────────────

// Linux on-demand USD/hour. Approximate list prices; update when AWS changes them.
var awsOnDemandPrices = map[string]map[string]float64{
	"eu-central-1": {
		"c7a.2xlarge": 0.4704,
		"c7a.4xlarge": 0.9408,
		"c7a.8xlarge": 1.8816,
		"c6a.4xlarge": 0.6984,
		"c7i.4xlarge": 0.8148,
		"c6i.4xlarge": 0.776,
		"m7a.4xlarge": 1.1128,
		"m6a.4xlarge": 0.8288,
	},
}

func (a *AWS) HourlyPrice(ctx context.Context, serverType, location string, spot bool) (float64, error) {
	if !spot {
		if p, ok := awsOnDemandPrices[a.Region][serverType]; ok {
			return p, nil
		}
		return 0, fmt.Errorf("aws: no on-demand price known for %s in %s", serverType, a.Region)
	}
	key := serverType + "|" + location
	a.mu.Lock()
	cached, ok := a.spotPrices[key]
	a.mu.Unlock()
	if ok && time.Since(cached.At) < awsSpotCacheTTL {
		return cached.Price, nil
	}
	c, err := a.ec2(ctx)
	if err != nil {
		return 0, err
	}
	in := &ec2.DescribeSpotPriceHistoryInput{
		InstanceTypes:       []types.InstanceType{types.InstanceType(serverType)},
		ProductDescriptions: []string{"Linux/UNIX"},
		StartTime:           aws.Time(time.Now()),
	}
	if location != "" {
		in.AvailabilityZone = aws.String(location)
	}
	res, err := c.DescribeSpotPriceHistory(ctx, in)
	if err != nil {
		return 0, awsError("spot price", err)
	}
	var latest *types.SpotPrice
	for i := range res.SpotPriceHistory {
		sp := &res.SpotPriceHistory[i]
		if latest == nil || aws.ToTime(sp.Timestamp).After(aws.ToTime(latest.Timestamp)) {
			latest = sp
		}
	}
	if latest == nil {
		return 0, fmt.Errorf("aws: no spot price for %s in %s", serverType, location)
	}
	price, err := strconv.ParseFloat(aws.ToString(latest.SpotPrice), 64)
	if err != nil {
		return 0, fmt.Errorf("aws: bad spot price %q: %w", aws.ToString(latest.SpotPrice), err)
	}
	a.mu.Lock()
	if a.spotPrices == nil {
		a.spotPrices = map[string]awsSpotPrice{}
	}
	a.spotPrices[key] = awsSpotPrice{Price: price, At: time.Now()}
	a.mu.Unlock()
	return price, nil
}

func (a *AWS) ValidateType(ctx context.Context, serverType string) error {
	c, err := a.ec2(ctx)
	if err != nil {
		return err
	}
	res, err := c.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{InstanceTypes: []types.InstanceType{types.InstanceType(serverType)}})
	if err != nil {
		return awsError("describe instance type "+serverType, err)
	}
	if len(res.InstanceTypes) == 0 {
		return fmt.Errorf("aws has no instance type %q in %s", serverType, a.Region)
	}
	return nil
}

// ── images ──────────────────────────────────────────────────────────────────

func awsImageTime(i *types.Image) time.Time {
	t, _ := time.Parse(time.RFC3339, aws.ToString(i.CreationDate))
	return t
}

func awsImage(i *types.Image) Image {
	img := Image{ID: aws.ToString(i.ImageId), Name: aws.ToString(i.Name), CreatedAt: awsImageTime(i)}
	for _, b := range i.BlockDeviceMappings {
		if b.Ebs != nil {
			img.SizeGB += float64(aws.ToInt32(b.Ebs.VolumeSize))
		}
	}
	return img
}

// BaseImage is the newest Canonical Ubuntu 24.04 amd64 AMI, looked up once a day.
func (a *AWS) BaseImage(ctx context.Context) (string, error) {
	a.mu.Lock()
	id, at := a.baseImage, a.baseAt
	a.mu.Unlock()
	if id != "" && time.Since(at) < 24*time.Hour {
		return id, nil
	}
	c, err := a.ec2(ctx)
	if err != nil {
		return "", err
	}
	res, err := c.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners:  []string{awsUbuntuOwner},
		Filters: []types.Filter{filter("name", awsUbuntuName), filter("architecture", "x86_64"), filter("state", "available")},
	})
	if err != nil {
		return "", awsError("describe Ubuntu images", err)
	}
	var newest *types.Image
	for i := range res.Images {
		if newest == nil || awsImageTime(&res.Images[i]).After(awsImageTime(newest)) {
			newest = &res.Images[i]
		}
	}
	if newest == nil {
		return "", fmt.Errorf("aws: no Ubuntu 24.04 image found in %s", a.Region)
	}
	id = aws.ToString(newest.ImageId)
	a.mu.Lock()
	a.baseImage, a.baseAt = id, time.Now()
	a.mu.Unlock()
	return id, nil
}

func (a *AWS) ListImages(ctx context.Context) ([]Image, error) {
	c, err := a.ec2(ctx)
	if err != nil {
		return nil, err
	}
	res, err := c.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners:  []string{"self"},
		Filters: []types.Filter{tagFilter(LabelImage, ImageKind), filter("state", "available")},
	})
	if err != nil {
		return nil, awsError("describe images", err)
	}
	out := make([]Image, 0, len(res.Images))
	for i := range res.Images {
		out = append(out, awsImage(&res.Images[i]))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (a *AWS) LatestImage(ctx context.Context) (*Image, error) {
	imgs, err := a.ListImages(ctx)
	if err != nil || len(imgs) == 0 {
		return nil, err
	}
	return &imgs[0], nil
}

func (a *AWS) CreateImage(ctx context.Context, instanceID, name string) (*Image, error) {
	c, err := a.ec2(ctx)
	if err != nil {
		return nil, err
	}
	tags := awsTags(map[string]string{LabelManaged: "true", LabelImage: ImageKind, "Name": name})
	res, err := c.CreateImage(ctx, &ec2.CreateImageInput{
		InstanceId: aws.String(instanceID),
		Name:       aws.String(name),
		NoReboot:   aws.Bool(true), // the instance is already stopped
		TagSpecifications: []types.TagSpecification{
			{ResourceType: types.ResourceTypeImage, Tags: tags},
			{ResourceType: types.ResourceTypeSnapshot, Tags: tags},
		},
	})
	if err != nil {
		return nil, awsError("create image", err)
	}
	id := aws.ToString(res.ImageId)
	desc := &ec2.DescribeImagesInput{ImageIds: []string{id}}
	out, err := ec2.NewImageAvailableWaiter(c).WaitForOutput(ctx, desc, 60*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("aws: wait for image %s: %w", id, err)
	}
	if len(out.Images) == 0 {
		return nil, fmt.Errorf("aws: image %s disappeared", id)
	}
	img := awsImage(&out.Images[0])
	return &img, nil
}

// DeleteImage deregisters the AMI and deletes its snapshots.
func (a *AWS) DeleteImage(ctx context.Context, id string) error {
	c, err := a.ec2(ctx)
	if err != nil {
		return err
	}
	res, err := c.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{id}})
	if awsCode(err) == "InvalidAMIID.NotFound" {
		return nil
	}
	if err != nil {
		return awsError("describe image", err)
	}
	var snaps []string
	for _, i := range res.Images {
		for _, b := range i.BlockDeviceMappings {
			if b.Ebs != nil && b.Ebs.SnapshotId != nil {
				snaps = append(snaps, *b.Ebs.SnapshotId)
			}
		}
	}
	if _, err := c.DeregisterImage(ctx, &ec2.DeregisterImageInput{ImageId: aws.String(id)}); err != nil && awsCode(err) != "InvalidAMIID.NotFound" {
		return awsError("deregister image", err)
	}
	for _, s := range snaps {
		_, err := c.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: aws.String(s)})
		if err != nil && awsCode(err) != "InvalidSnapshot.NotFound" {
			return awsError("delete snapshot "+s, err)
		}
	}
	a.mu.Lock()
	delete(a.rootDevice, id)
	a.mu.Unlock()
	return nil
}

func shutdownBehavior(stop bool) types.ShutdownBehavior {
	if stop {
		return types.ShutdownBehaviorStop
	}
	return types.ShutdownBehaviorTerminate
}
