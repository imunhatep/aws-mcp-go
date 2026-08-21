package mcpserver

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	ptypes "github.com/imunhatep/awslib/provider/types"
	"github.com/imunhatep/awslib/proxy"
	"github.com/imunhatep/awslib/resources"
	"github.com/imunhatep/awslib/service"

	awscfg "github.com/aws/aws-sdk-go-v2/service/configservice/types"

	"github.com/imunhatep/aws-mcp-go/pkg/errors"
)

// defaultPageLimit and maxPageLimit bound how many resources a single
// list_resources page returns, keeping responses under the MCP token cap
// regardless of inventory size.
const (
	defaultPageLimit = 50
	maxPageLimit     = 1000
)

// resolveRegions turns the region argument into the concrete list of regions to
// query: a single validated region, or every known region when empty.
func resolveRegions(regionArg string) ([]ptypes.AwsRegion, error) {
	if regionArg == "" {
		return ptypes.GetAwsRegionList(), nil
	}
	if _, known := ptypes.GetAwsRegionData()[ptypes.AwsRegion(regionArg)]; !known {
		return nil, errors.Errorf("unknown region %q; call list_regions for valid values", regionArg)
	}
	return []ptypes.AwsRegion{ptypes.AwsRegion(regionArg)}, nil
}

// fetchResources drives the awslib proxy/provider pipeline: it resolves clients
// for the regions, wires the cache, and reads every resource of the type. Both
// list_resources and count_resources share this.
// fetchResources runs the typed resource path over the pool. A non-empty
// accountID scopes the fan-out to that one account rather than filtering its
// rows out afterwards — see poolClients.
func (s *Server) fetchResources(rt awscfg.ResourceType, regions []ptypes.AwsRegion, accountID string) ([]service.ResourceInterface, error) {
	clients, err := s.poolClients(accountID, regions)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	proxyPool := proxy.NewRepoProxyPool(s.ctx, clients)
	if s.cache != nil {
		proxyPool = proxyPool.WithCache(s.cache)
	}

	return resources.NewProvider(rt, proxyPool.List(rt)...).Run().Read(), nil
}

// resourceFilter is the set of server-side predicates applied before a resource
// is serialized, so payload scales with the answer rather than the inventory.
type resourceFilter struct {
	accountID string // exact account match
	state     string // case-insensitive lifecycle state/status match

	tagKey, tagVal   string
	hasTag           bool
	attrKey, attrVal string
	hasAttr          bool
}

// parseFilter builds a resourceFilter from the raw tool arguments. tag and
// attribute use "key=value" form.
func parseFilter(accountID, state, tag, attribute string) (resourceFilter, error) {
	f := resourceFilter{accountID: strings.TrimSpace(accountID), state: strings.TrimSpace(state)}

	if tag = strings.TrimSpace(tag); tag != "" {
		k, v, ok := strings.Cut(tag, "=")
		if !ok {
			return f, errors.Errorf("invalid tag filter %q; expected key=value", tag)
		}
		f.tagKey, f.tagVal, f.hasTag = strings.TrimSpace(k), strings.TrimSpace(v), true
	}

	if attribute = strings.TrimSpace(attribute); attribute != "" {
		k, v, ok := strings.Cut(attribute, "=")
		if !ok {
			return f, errors.Errorf("invalid attribute filter %q; expected key=value", attribute)
		}
		f.attrKey, f.attrVal, f.hasAttr = strings.TrimSpace(k), strings.TrimSpace(v), true
	}

	return f, nil
}

// matches reports whether a resource (with its precomputed attributes) passes
// every configured predicate.
func (f resourceFilter) matches(r service.ResourceInterface, attrs map[string]any) bool {
	if f.accountID != "" && r.GetAccountID().String() != f.accountID {
		return false
	}
	if f.state != "" && !strings.EqualFold(resourceState(attrs), f.state) {
		return false
	}
	if f.hasTag && r.GetTags()[f.tagKey] != f.tagVal {
		return false
	}
	if f.hasAttr {
		v, ok := attrs[f.attrKey]
		if !ok || fmt.Sprint(v) != f.attrVal {
			return false
		}
	}
	return true
}

// parseCursor decodes an opaque page cursor into a zero-based offset. An empty
// cursor is the first page.
func parseCursor(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, errors.Errorf("invalid cursor %q", raw)
	}
	return n, nil
}

// clampLimit applies the default and ceiling to the requested page size.
func clampLimit(limit int) int {
	if limit <= 0 {
		return defaultPageLimit
	}
	if limit > maxPageLimit {
		return maxPageLimit
	}
	return limit
}

// listResult is the paginated envelope returned by list_resources. Count is the
// rows in this page; Total is the count after filtering across all pages;
// NextCursor is set only when more rows remain.
type listResult struct {
	Items      []resourceDTO `json:"items"`
	Count      int           `json:"count"`
	Total      int           `json:"total"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// dtoSortKey gives a stable ordering so pagination is deterministic across
// calls: by ARN, then id, then name.
func dtoSortKey(d resourceDTO) string {
	return d.Arn + "\x00" + d.ID + "\x00" + d.Name
}

// paginate sorts, offsets and truncates the DTOs into a single page envelope.
func paginate(items []resourceDTO, offset, limit int) listResult {
	sort.Slice(items, func(i, j int) bool { return dtoSortKey(items[i]) < dtoSortKey(items[j]) })

	total := len(items)
	offset = min(offset, total)
	end := min(offset+limit, total)

	page := items[offset:end]
	res := listResult{Items: page, Count: len(page), Total: total}
	if end < total {
		res.NextCursor = strconv.Itoa(end)
	}
	return res
}

// countBucket is one group in a count_resources result.
type countBucket struct {
	Group map[string]string `json:"group"`
	Count int               `json:"count"`
}

// countResult is the aggregate envelope returned by count_resources.
type countResult struct {
	Total   int           `json:"total"`
	GroupBy []string      `json:"group_by"`
	Buckets []countBucket `json:"buckets"`
}

// parseGroupBy splits and validates the comma-separated group_by dimensions.
// Supported: type, state, region, account_id, tag:<key>, attr:<key>. Empty
// defaults to grouping by state.
func parseGroupBy(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []string{"state"}, nil
	}

	var dims []string
	for d := range strings.SplitSeq(raw, ",") {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		switch {
		case d == "type" || d == "state" || d == "region" || d == "account_id":
		case strings.HasPrefix(d, "tag:") && len(d) > 4:
		case strings.HasPrefix(d, "attr:") && len(d) > 5:
		default:
			return nil, errors.Errorf("invalid group_by dimension %q; use type, state, region, account_id, tag:<key> or attr:<key>", d)
		}
		dims = append(dims, d)
	}

	if len(dims) == 0 {
		return []string{"state"}, nil
	}
	return dims, nil
}

// dimValue extracts the value of a single group_by dimension for a resource.
func dimValue(dim string, r service.ResourceInterface, attrs map[string]any) string {
	switch {
	case dim == "type":
		return string(r.GetType())
	case dim == "state":
		return resourceState(attrs)
	case dim == "region":
		return r.GetRegion().String()
	case dim == "account_id":
		return r.GetAccountID().String()
	case strings.HasPrefix(dim, "tag:"):
		return r.GetTags()[dim[len("tag:"):]]
	case strings.HasPrefix(dim, "attr:"):
		if v, ok := attrs[dim[len("attr:"):]]; ok {
			return fmt.Sprint(v)
		}
	}
	return ""
}

// aggregate groups resources by the given dimensions and returns bucket counts
// sorted by descending count (ties broken by group key for stable output).
func aggregate(items []service.ResourceInterface, filter resourceFilter, dims []string) countResult {
	type entry struct {
		group map[string]string
		count int
	}
	buckets := map[string]*entry{}
	total := 0

	for _, r := range items {
		attrs := summaryAttributes(r)
		if !filter.matches(r, attrs) {
			continue
		}
		total++

		group := make(map[string]string, len(dims))
		keyParts := make([]string, len(dims))
		for i, dim := range dims {
			v := dimValue(dim, r, attrs)
			if v == "" {
				v = "(none)"
			}
			group[dim] = v
			keyParts[i] = v
		}
		key := strings.Join(keyParts, "\x1f")

		if b, ok := buckets[key]; ok {
			b.count++
		} else {
			buckets[key] = &entry{group: group, count: 1}
		}
	}

	out := make([]countBucket, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, countBucket{Group: b.group, Count: b.count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return groupKey(out[i].Group, dims) < groupKey(out[j].Group, dims)
	})

	return countResult{Total: total, GroupBy: dims, Buckets: out}
}

// groupKey renders a bucket's group as a stable string for tie-breaking.
func groupKey(group map[string]string, dims []string) string {
	parts := make([]string, len(dims))
	for i, d := range dims {
		parts[i] = group[d]
	}
	return strings.Join(parts, "\x1f")
}
