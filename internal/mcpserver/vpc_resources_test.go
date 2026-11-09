package mcpserver

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/service/configservice/types"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ptypes "github.com/imunhatep/awslib/provider/types"
	"github.com/imunhatep/awslib/service"
	"github.com/imunhatep/awslib/service/cfg"
	"github.com/imunhatep/awslib/service/ec2"
)

// vpcNetworkingTypes are the EC2/VPC types wired into awslib's FindAll dispatch
// that the allowlist must expose. They were dispatchable upstream long before
// they were listed here, and every request for one silently fell through to the
// Cloud Control fallback — a weaker answer from a slower path.
var vpcNetworkingTypes = []awscfg.ResourceType{
	awscfg.ResourceTypeVpc,
	awscfg.ResourceTypeSubnet,
	awscfg.ResourceTypeSecurityGroup,
	awscfg.ResourceTypeVPCEndpoint,
	awscfg.ResourceTypeRouteTable,
	awscfg.ResourceTypeEip,
	awscfg.ResourceTypeNetworkInterface,
}

// TestVpcNetworkingTypesSupported pins the allowlist against the drift that sent
// Elastic IP queries down the fallback path.
func TestVpcNetworkingTypesSupported(t *testing.T) {
	for _, rt := range vpcNetworkingTypes {
		t.Run(string(rt), func(t *testing.T) {
			assert.True(t, isSupported(rt), "type must be in SupportedResourceTypes")

			got, ok := ResolveResourceType(string(rt))
			require.True(t, ok, "canonical form must resolve")
			assert.Equal(t, rt, got)

			// The URL form is the spelling the tool description advertises, so
			// it has to resolve to the same type.
			url := cfg.ResourceTypeToUrl(rt)
			require.NotEmpty(t, url, "awslib must know a URL form for this type")

			got, ok = ResolveResourceType(url)
			require.True(t, ok, "URL form %q must resolve", url)
			assert.Equal(t, rt, got)
		})
	}
}

// TestVpcNetworkingTypesAreRegional guards the fan-out: a type wrongly treated
// as global is fetched from one region only, so resources elsewhere vanish.
func TestVpcNetworkingTypesAreRegional(t *testing.T) {
	global := map[awscfg.ResourceType]struct{}{}
	for _, rt := range cfg.ResourceTypeListGlobal() {
		global[rt] = struct{}{}
	}

	for _, rt := range vpcNetworkingTypes {
		_, isGlobal := global[rt]
		assert.False(t, isGlobal, "%s is regional and must not be collapsed to one region", rt)
	}
}

func sampleAddress(associated bool) ec2.Address {
	address := ec2types.Address{
		AllocationId:       aws.String("eipalloc-0123456789abcdef0"),
		PublicIp:           aws.String("52.0.0.1"),
		NetworkBorderGroup: aws.String("eu-north-1"),
		Domain:             ec2types.DomainTypeVpc,
		Tags: []ec2types.Tag{
			{Key: aws.String("Name"), Value: aws.String("nat-egress")},
		},
	}

	if associated {
		address.AssociationId = aws.String("eipassoc-0123456789abcdef0")
		address.NetworkInterfaceId = aws.String("eni-0123456789abcdef0")
		address.PrivateIpAddress = aws.String("10.0.0.5")
	}

	return ec2.Address{
		AbstractResource: service.AbstractResource{
			AccountID: ptypes.AwsAccountID("111111111111"),
			Region:    ptypes.AwsRegion("eu-north-1"),
			ID:        "eipalloc-0123456789abcdef0",
			Type:      awscfg.ResourceTypeEip,
		},
		Address: address,
	}
}

// TestSummaryAttributes_Eip covers the attributes the Cloud Control fallback
// could not report: what the address is attached to, and whether it is attached
// at all. An idle Elastic IP bills for nothing, so that is the question asked of
// this type more than any other.
func TestSummaryAttributes_Eip(t *testing.T) {
	t.Run("associated", func(t *testing.T) {
		attrs := summaryAttributes(sampleAddress(true))
		require.NotNil(t, attrs)

		assert.Equal(t, "52.0.0.1", attrs["public_ip"])
		assert.Equal(t, "10.0.0.5", attrs["private_ip"])
		assert.Equal(t, "eipalloc-0123456789abcdef0", attrs["allocation_id"])
		assert.Equal(t, "eipassoc-0123456789abcdef0", attrs["association_id"])
		assert.Equal(t, "eni-0123456789abcdef0", attrs["network_interface_id"])
		assert.Equal(t, "eu-north-1", attrs["network_border_group"])
		assert.Equal(t, "vpc", attrs["domain"])
		assert.Equal(t, true, attrs["associated"])
		assert.Equal(t, "associated", attrs["state"])
	})

	t.Run("unassociated", func(t *testing.T) {
		attrs := summaryAttributes(sampleAddress(false))
		require.NotNil(t, attrs)

		assert.Equal(t, false, attrs["associated"])
		assert.Equal(t, "unassociated", attrs["state"],
			"an idle Elastic IP must be groupable and filterable by state")
		assert.NotContains(t, attrs, "association_id", "empty attributes are dropped, not reported as empty")
	})
}

// TestResourceDTO_Eip checks the whole DTO path for an Elastic IP: the derived
// state reaches the thin view, and tags — which Cloud Control never returned for
// this type — reach the summary view.
func TestResourceDTO_Eip(t *testing.T) {
	dto := toResourceDTO(sampleAddress(true), viewID)
	assert.Equal(t, "associated", dto.State)
	assert.Equal(t, "nat-egress", dto.Name, "the Name tag identifies an EIP")
	assert.Nil(t, dto.Tags, "tags are omitted in the id view")

	dto = toResourceDTO(sampleAddress(true), viewSummary)
	require.NotNil(t, dto.Tags)
	assert.Equal(t, "nat-egress", dto.Tags["Name"])
}

// TestSummaryAttributes_VpcNetworking checks that the other four types report
// curated attributes rather than falling through to nil, which is what makes the
// state/attribute filters and every group_by dimension work for them.
func TestSummaryAttributes_VpcNetworking(t *testing.T) {
	base := service.AbstractResource{
		AccountID: ptypes.AwsAccountID("111111111111"),
		Region:    ptypes.AwsRegion("eu-north-1"),
	}

	t.Run("subnet", func(t *testing.T) {
		attrs := summaryAttributes(ec2.Subnet{
			AbstractResource: base,
			Subnet: ec2types.Subnet{
				VpcId:                   aws.String("vpc-01"),
				CidrBlock:               aws.String("10.0.1.0/24"),
				AvailabilityZone:        aws.String("eu-north-1a"),
				State:                   ec2types.SubnetStateAvailable,
				MapPublicIpOnLaunch:     aws.Bool(true),
				AvailableIpAddressCount: aws.Int32(250),
			},
		})

		require.NotNil(t, attrs)
		assert.Equal(t, "vpc-01", attrs["vpc_id"])
		assert.Equal(t, "10.0.1.0/24", attrs["cidr_block"])
		assert.Equal(t, "available", attrs["state"])
		assert.Equal(t, true, attrs["is_public"])
		assert.Equal(t, int32(250), attrs["available_ip_count"])
	})

	t.Run("security group", func(t *testing.T) {
		attrs := summaryAttributes(ec2.SecurityGroup{
			AbstractResource: base,
			SecurityGroup: ec2types.SecurityGroup{
				GroupName:     aws.String("web"),
				VpcId:         aws.String("vpc-01"),
				Description:   aws.String("web tier"),
				IpPermissions: []ec2types.IpPermission{{}, {}},
			},
		})

		require.NotNil(t, attrs)
		assert.Equal(t, "web", attrs["group_name"])
		assert.Equal(t, 2, attrs["ingress_rules"])
		assert.Equal(t, 0, attrs["egress_rules"])
	})

	t.Run("vpc endpoint", func(t *testing.T) {
		attrs := summaryAttributes(ec2.VpcEndpoint{
			AbstractResource: base,
			VpcEndpoint: ec2types.VpcEndpoint{
				VpcId:           aws.String("vpc-01"),
				ServiceName:     aws.String("com.amazonaws.eu-north-1.s3"),
				VpcEndpointType: ec2types.VpcEndpointTypeGateway,
				State:           ec2types.StateAvailable,
				SubnetIds:       []string{"subnet-01"},
			},
		})

		require.NotNil(t, attrs)
		assert.Equal(t, "com.amazonaws.eu-north-1.s3", attrs["service_name"])
		assert.Equal(t, "Gateway", attrs["endpoint_type"])
		// The SDK spells VPC-endpoint state "Available"; the state filter compares
		// case-insensitively, so the provider spelling is kept as-is.
		assert.Equal(t, "Available", attrs["state"])
		assert.Equal(t, 1, attrs["subnets"])
	})

	t.Run("route table", func(t *testing.T) {
		attrs := summaryAttributes(ec2.RouteTable{
			AbstractResource: base,
			RouteTable: ec2types.RouteTable{
				VpcId:  aws.String("vpc-01"),
				Routes: []ec2types.Route{{}, {}, {}},
				Associations: []ec2types.RouteTableAssociation{
					{Main: aws.Bool(true)},
				},
			},
		})

		require.NotNil(t, attrs)
		assert.Equal(t, "vpc-01", attrs["vpc_id"])
		assert.Equal(t, true, attrs["is_main"])
		assert.Equal(t, 3, attrs["routes"])
	})
}

// nlbNetworkInterface is one AZ's interface of an internet-facing NLB with no
// Elastic IP: DescribeLoadBalancers reports no address for it, and Cloud
// Control's AWS::EC2::NetworkInterface carries no public address either, so this
// entity is the only place the frontend IP can come from.
func nlbNetworkInterface() ec2.NetworkInterface {
	return ec2.NetworkInterface{
		AbstractResource: service.AbstractResource{
			AccountID: ptypes.AwsAccountID("111111111111"),
			Region:    ptypes.AwsRegion("af-south-1"),
			ID:        "eni-092a6e24ee8701c61",
			Type:      awscfg.ResourceTypeNetworkInterface,
		},
		NetworkInterface: ec2types.NetworkInterface{
			NetworkInterfaceId: aws.String("eni-092a6e24ee8701c61"),
			Description:        aws.String("ELB net/ingress/818502e1be7cef4d"),
			InterfaceType:      ec2types.NetworkInterfaceTypeNetworkLoadBalancer,
			Status:             ec2types.NetworkInterfaceStatusInUse,
			RequesterManaged:   aws.Bool(true),
			PrivateIpAddress:   aws.String("10.17.204.134"),
			Association:        &ec2types.NetworkInterfaceAssociation{PublicIp: aws.String("15.240.22.35")},
			PrivateIpAddresses: []ec2types.NetworkInterfacePrivateIpAddress{{
				Primary:          aws.Bool(true),
				PrivateIpAddress: aws.String("10.17.204.134"),
				Association:      &ec2types.NetworkInterfaceAssociation{PublicIp: aws.String("15.240.22.35")},
			}},
			VpcId:            aws.String("vpc-0fe2306fe8af32c4a"),
			SubnetId:         aws.String("subnet-050b1092769a6e9b0"),
			AvailabilityZone: aws.String("af-south-1c"),
		},
	}
}

func TestSummaryAttributes_NetworkInterface(t *testing.T) {
	t.Run("nlb frontend with an auto-assigned address", func(t *testing.T) {
		attrs := summaryAttributes(nlbNetworkInterface())
		require.NotNil(t, attrs)

		assert.Equal(t, "15.240.22.35", attrs["public_ip"])
		assert.Equal(t, "10.17.204.134", attrs["private_ip"])
		assert.Equal(t, false, attrs["elastic_ip"], "an auto-assigned address must not read as an Elastic IP")
		assert.Equal(t, "network_load_balancer", attrs["interface_type"])
		assert.Equal(t, "ELB net/ingress/818502e1be7cef4d", attrs["description"],
			"description is how a caller finds one load balancer's interfaces")
		assert.Equal(t, "in-use", attrs["state"])
		assert.Equal(t, true, attrs["requester_managed"])
		assert.Equal(t, "af-south-1c", attrs["availability_zone"])
		assert.NotContains(t, attrs, "public_ips", "a single address is not repeated as a list")
	})

	t.Run("instance interface with secondary addresses", func(t *testing.T) {
		eni := nlbNetworkInterface()
		eni.InterfaceType = ec2types.NetworkInterfaceTypeInterface
		eni.RequesterManaged = aws.Bool(false)
		eni.Association.AllocationId = aws.String("eipalloc-1")
		eni.Attachment = &ec2types.NetworkInterfaceAttachment{InstanceId: aws.String("i-1")}
		eni.PrivateIpAddresses = append(eni.PrivateIpAddresses, ec2types.NetworkInterfacePrivateIpAddress{
			PrivateIpAddress: aws.String("10.17.204.135"),
			Association:      &ec2types.NetworkInterfaceAssociation{PublicIp: aws.String("15.240.22.36")},
		})

		attrs := summaryAttributes(eni)
		require.NotNil(t, attrs)

		assert.Equal(t, true, attrs["elastic_ip"])
		assert.Equal(t, "i-1", attrs["instance_id"])
		assert.Equal(t, []string{"10.17.204.134", "10.17.204.135"}, attrs["private_ips"])
		assert.Equal(t, []string{"15.240.22.35", "15.240.22.36"}, attrs["public_ips"])
	})

	t.Run("private only", func(t *testing.T) {
		eni := nlbNetworkInterface()
		eni.Association = nil
		eni.PrivateIpAddresses[0].Association = nil

		attrs := summaryAttributes(eni)
		assert.NotContains(t, attrs, "public_ip")
		assert.NotContains(t, attrs, "elastic_ip", "elastic_ip only qualifies an address that exists")
	})
}

// The question this type answers is "what are this load balancer's IPs", so the
// description filter has to select exactly its interfaces.
func TestNetworkInterfaceFilterByDescription(t *testing.T) {
	eni := nlbNetworkInterface()
	other := nlbNetworkInterface()
	other.Description = aws.String("Interface for NAT Gateway nat-1")

	filter := resourceFilter{hasAttr: true, attrKey: "description", attrVal: "ELB net/ingress/818502e1be7cef4d"}
	assert.True(t, filter.matches(eni, summaryAttributes(eni)))
	assert.False(t, filter.matches(other, summaryAttributes(other)))
}

// A global type with no region must not sweep every region: awslib answers it
// from us-east-1 alone, and building a client per (account, region) first is
// what made count_resources on CloudFront distributions time out.
func TestResolveTypeRegions(t *testing.T) {
	regions, err := resolveTypeRegions(awscfg.ResourceTypeDistribution, "")
	require.NoError(t, err)
	assert.Equal(t, []ptypes.AwsRegion{ptypes.DefaultAwsRegion}, regions)

	regions, err = resolveTypeRegions(awscfg.ResourceTypeNetworkInterface, "")
	require.NoError(t, err)
	assert.Greater(t, len(regions), 1, "a regional type still sweeps every region")

	regions, err = resolveTypeRegions(awscfg.ResourceTypeDistribution, "eu-west-1")
	require.NoError(t, err)
	assert.Equal(t, []ptypes.AwsRegion{"eu-west-1"}, regions, "an explicit region is honoured")

	_, err = resolveTypeRegions(awscfg.ResourceTypeDistribution, "mars-1")
	assert.Error(t, err, "region validation still applies")
}
