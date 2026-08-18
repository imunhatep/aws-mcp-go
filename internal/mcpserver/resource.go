package mcpserver

import (
	"encoding/json"
	"strings"
	"time"

	awscfg "github.com/aws/aws-sdk-go-v2/service/configservice/types"
	"github.com/imunhatep/gocollection/slice"
	"github.com/rs/zerolog/log"

	"github.com/imunhatep/awslib/service"
	"github.com/imunhatep/awslib/service/cfg"
)

// SupportedResourceTypes returns the resource types the MCP server is able to
// list. The list mirrors the dispatch switch in proxy.RepoProxy.FindAll — types
// present in cfg.ResourceTypeList() but not wired into FindAll (e.g. Athena,
// CloudTrail) are intentionally excluded so callers never hit a
// "resource type not supported" error. Same reason CloudFront is represented by
// DistributionTenantSummary rather than the full DistributionTenant: only the
// summary form is listable — the full tenant comes from a per-identifier Get.
func SupportedResourceTypes() []awscfg.ResourceType {
	return []awscfg.ResourceType{
		// asg
		awscfg.ResourceTypeAutoScalingGroup,
		// batch
		awscfg.ResourceTypeBatchComputeEnvironment,
		awscfg.ResourceTypeBatchJobQueue,
		// glue
		cfg.ResourceTypeGlueDatabase,
		awscfg.ResourceTypeGlueJob,
		cfg.ResourceTypeGlueTable,
		// s3
		awscfg.ResourceTypeBucket,
		// rds
		awscfg.ResourceTypeDBInstance,
		awscfg.ResourceTypeDBSnapshot,
		// dynamodb
		awscfg.ResourceTypeTable,
		// ecs
		awscfg.ResourceTypeECSCluster,
		awscfg.ResourceTypeECSService,
		// eks
		awscfg.ResourceTypeEKSCluster,
		// emr
		cfg.ResourceTypeEmrCluster,
		cfg.ResourceTypeEmrServerlessApplication,
		cfg.ResourceTypeEmrServerlessJobRun,
		// lambda
		awscfg.ResourceTypeFunction,
		// ec2
		awscfg.ResourceTypeInstance,
		awscfg.ResourceTypeVolume,
		cfg.ResourceTypeSnapshot,
		awscfg.ResourceTypeVpc,
		// cloudfront (SaaS Manager; global control plane)
		cfg.ResourceTypeCloudFrontDistributionTenantSummary,
		cfg.ResourceTypeCloudFrontConnectionGroup,
		// cloudwatch
		cfg.ResourceTypeCloudWatchLogGroup,
		// route53
		awscfg.ResourceTypeRoute53HostedZone,
		cfg.ResourceTypeRoute53DomainSummary,
		cfg.ResourceTypeRoute53Domain,
		cfg.ResourceTypeRoute53ResourceRecord,
		// efs
		awscfg.ResourceTypeEFSFileSystem,
		// elb
		awscfg.ResourceTypeLoadBalancerV2,
		// secretsmanager
		awscfg.ResourceTypeSecret,
		// sqs
		awscfg.ResourceTypeQueue,
		// sns
		awscfg.ResourceTypeTopic,
		// iam
		awscfg.ResourceTypeUser,
	}
}

// isSupported reports whether the given resource type is handled by FindAll.
func isSupported(rt awscfg.ResourceType) bool {
	return slice.Contains(SupportedResourceTypes(), rt)
}

// ResolveResourceType accepts either the canonical CloudFormation form
// ("AWS::EC2::Instance", case-insensitive) or the URL form ("aws_ec2_instance")
// and returns the matching supported resource type.
func ResolveResourceType(raw string) (awscfg.ResourceType, bool) {
	raw = strings.TrimSpace(raw)

	// URL form (e.g. "aws_ec2_instance")
	if rt, ok := cfg.ResourceTypeFromUrl(strings.ToLower(raw)); ok && isSupported(rt) {
		return rt, true
	}

	// canonical form, case-insensitive
	for _, rt := range SupportedResourceTypes() {
		if strings.EqualFold(string(rt), raw) {
			return rt, true
		}
	}

	return "", false
}

// resourceTypeInfo describes a resource type for the list_resource_types tool.
type resourceTypeInfo struct {
	Type   string `json:"type"`
	URL    string `json:"url"`
	Global bool   `json:"global"`
}

func supportedResourceTypeInfos() []resourceTypeInfo {
	global := cfg.ResourceTypeListGlobal()

	infos := make([]resourceTypeInfo, 0, len(SupportedResourceTypes()))
	for _, rt := range SupportedResourceTypes() {
		infos = append(infos, resourceTypeInfo{
			Type:   string(rt),
			URL:    cfg.ResourceTypeToUrl(rt),
			Global: slice.Contains(global, rt),
		})
	}

	return infos
}

// resourceView controls how much of a resource the list_resources tool returns
// per row, trading payload size against richness.
type resourceView string

const (
	// viewID is the thin default: identity fields plus state only. Hundreds of
	// rows fit comfortably under the MCP response limit.
	viewID resourceView = "id"
	// viewSummary adds tags and the curated attributes map.
	viewSummary resourceView = "summary"
	// viewDetail adds the full provider-native entity under raw. Intended for a
	// single resource or a small, filtered set.
	viewDetail resourceView = "detail"
)

// parseView resolves the view argument, defaulting to the thin viewID.
func parseView(raw string) (resourceView, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(viewID):
		return viewID, true
	case string(viewSummary):
		return viewSummary, true
	case string(viewDetail), "raw", "full":
		return viewDetail, true
	default:
		return "", false
	}
}

// resourceDTO is the JSON representation of a single AWS resource returned by
// the list_resources tool. It is built from the normalized
// service.ResourceInterface so all resource types serialize uniformly.
//
// The fields returned scale with the requested view: viewID emits only the
// identity fields plus State; viewSummary adds Tags and the curated Attributes;
// viewDetail additionally embeds the full concrete entity under Raw — including
// the embedded AWS SDK struct (e.g. an EC2 instance's block-device mappings)
// that the curated fields do not surface.
type resourceDTO struct {
	AccountID string `json:"account_id"`
	Region    string `json:"region"`
	Type      string `json:"type"`
	Arn       string `json:"arn,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	// State is the resource's lifecycle state/status when it has one (e.g. an
	// EC2 instance's "running", an RDS instance's "available"), lifted from the
	// curated attributes so it is present even in the thin viewID.
	State     string            `json:"state,omitempty"`
	CreatedAt string            `json:"created_at,omitempty"`
	Tags      map[string]string `json:"tags,omitempty"`
	// Attributes holds a compact, curated set of the most relevant
	// provider-native fields for recognized resource types (e.g. an EC2
	// instance's type/state/IPs). Nil for types without a curated summary.
	Attributes map[string]any  `json:"attributes,omitempty"`
	Raw        json.RawMessage `json:"raw,omitempty"`
}

// resourceState lifts a lifecycle state/status out of the curated attributes,
// so callers can filter and group on it uniformly across resource types.
func resourceState(attrs map[string]any) string {
	for _, k := range []string{"state", "status"} {
		if v, ok := attrs[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// toResourceDTO maps a resource to its DTO at the given view. It is a thin
// wrapper over buildResourceDTO that computes the curated attributes itself;
// hot paths that already have the attributes (filtering, grouping) call
// buildResourceDTO directly to avoid recomputing them.
func toResourceDTO(r service.ResourceInterface, view resourceView) resourceDTO {
	return buildResourceDTO(r, summaryAttributes(r), view)
}

// buildResourceDTO assembles the DTO from a resource and its precomputed curated
// attributes. Marshalling the interface value for viewDetail serializes the
// underlying concrete type (e.g. ec2.Instance), which embeds the complete AWS
// SDK struct — so every field awslib fetched is preserved, for any resource
// type, with no per-service code.
func buildResourceDTO(r service.ResourceInterface, attrs map[string]any, view resourceView) resourceDTO {
	dto := resourceDTO{
		AccountID: r.GetAccountID().String(),
		Region:    r.GetRegion().String(),
		Type:      string(r.GetType()),
		Arn:       r.GetArn(),
		ID:        r.GetId(),
		Name:      r.GetName(),
		State:     resourceState(attrs),
	}

	if createdAt := r.GetCreatedAt(); !createdAt.IsZero() {
		dto.CreatedAt = createdAt.Format(time.RFC3339)
	}

	if view == viewSummary || view == viewDetail {
		dto.Tags = r.GetTags()
		dto.Attributes = attrs
	}

	if view == viewDetail {
		if raw, err := json.Marshal(r); err != nil {
			log.Warn().Err(err).Str("id", r.GetIdOrArn()).Msg("[mcpserver.buildResourceDTO] failed to marshal full resource detail")
		} else {
			dto.Raw = raw
		}
	}

	return dto
}
