// Package registry implements a minimal OCI Distribution API client used to
// discover the newest tag of an image repository (GET /v2/<repo>/tags/list).
// It supports the anonymous Docker Hub bearer-token dance and plain-HTTP
// local registries (e.g. host.docker.internal:5050). The tags/list endpoint
// carries no timestamps, so "newest" is approximated by the highest
// version-like tag (semver-aware: 1.10 > 1.9), with "latest" as the fallback
// when no version-like tags exist.
//
// The queried host always comes from the image reference in the Deployment
// spec (operator-controlled), never from LLM output, and only /v2/ GET
// endpoints are touched with anonymous credentials.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// dockerHubHost is the v2 API endpoint behind the docker.io aliases.
const dockerHubHost = "registry-1.docker.io"

// Client queries an OCI registry. The zero value is usable: HTTPS is tried
// first, then plain HTTP (local registries), each with a short timeout so a
// dead registry never stalls the poll loop.
type Client struct {
	// HTTP overrides the underlying client (tests). Nil uses a 5s-timeout default.
	HTTP *http.Client
	// Schemes lists the URL schemes tried in order. Nil means ["https", "http"].
	Schemes []string
}

// Default is the client used by the package-level helpers.
var Default = &Client{}

// NewestTag resolves the newest tag of image's repository, excluding the tag
// the image currently carries (it is the broken one that triggered the
// lookup). The whole discovery is capped at 8 seconds.
func NewestTag(ctx context.Context, image string) (string, error) {
	return Default.NewestTag(ctx, image)
}

// NewestTag implements the package-level helper on a configurable client.
func (c *Client) NewestTag(ctx context.Context, image string) (string, error) {
	host, repo, currentTag, err := ParseRef(image)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	tags, err := c.listTags(ctx, host, repo)
	if err != nil {
		return "", fmt.Errorf("registry %s: %w", host, err)
	}
	tag := PickNewest(tags, currentTag)
	if tag == "" {
		return "", fmt.Errorf("registry %s: no candidate tag among %d tags of %s", host, len(tags), repo)
	}
	return tag, nil
}

// ParseRef splits an image reference into registry host, repository path and
// tag. References without an explicit registry resolve to Docker Hub, with
// the implicit library/ namespace for single-segment names. Digests are
// dropped: the caller is replacing the reference anyway.
func ParseRef(image string) (host, repo, tag string, err error) {
	image = strings.TrimSpace(image)
	if image == "" {
		return "", "", "", fmt.Errorf("image is empty")
	}
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	slash := strings.LastIndex(image, "/")
	if colon := strings.LastIndex(image, ":"); colon > slash {
		tag = image[colon+1:]
		image = image[:colon]
	}

	first, rest := image, ""
	if idx := strings.Index(image, "/"); idx >= 0 {
		first, rest = image[:idx], image[idx+1:]
	}
	// A leading segment is a registry host only when it looks like one
	// (contains "." or ":", or is "localhost"); otherwise it is a repo
	// namespace on Docker Hub. Same convention as kube.SwapRegistry.
	if rest != "" && (strings.ContainsAny(first, ".:") || first == "localhost") {
		host, repo = first, rest
	} else {
		host, repo = dockerHubHost, image
	}
	if host == "docker.io" || host == "index.docker.io" {
		host = dockerHubHost
	}
	if host == dockerHubHost && !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	if repo == "" {
		return "", "", "", fmt.Errorf("image reference %q has no repository", image)
	}
	return host, repo, tag, nil
}

// listTags fetches the tag list, trying each scheme in order. A single page
// of up to 1000 tags is requested; repositories beyond that are vanishingly
// rare for the workloads this agent targets.
func (c *Client) listTags(ctx context.Context, host, repo string) ([]string, error) {
	schemes := c.Schemes
	if len(schemes) == 0 {
		schemes = []string{"https", "http"}
	}
	httpc := c.HTTP
	if httpc == nil {
		httpc = &http.Client{Timeout: 5 * time.Second}
	}

	var lastErr error
	for _, scheme := range schemes {
		u := scheme + "://" + host + "/v2/" + repo + "/tags/list?n=1000"
		tags, err := c.fetchTags(ctx, httpc, u, "")
		if err == nil {
			return tags, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// fetchTags performs one tags/list request, transparently handling the
// anonymous bearer-token challenge Docker Hub (and friends) answer with.
func (c *Client) fetchTags(ctx context.Context, httpc *http.Client, u, bearer string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized && bearer == "" {
		token, terr := c.fetchToken(ctx, httpc, resp.Header.Get("WWW-Authenticate"))
		if terr != nil {
			return nil, fmt.Errorf("anonymous auth failed: %w", terr)
		}
		return c.fetchTags(ctx, httpc, u, token)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("tags/list returned HTTP %d", resp.StatusCode)
	}

	var out struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Tags, nil
}

// bearerParamRe extracts key="value" pairs from a WWW-Authenticate header.
var bearerParamRe = regexp.MustCompile(`(\w+)="([^"]*)"`)

// fetchToken requests an anonymous pull token from the realm advertised in
// the Bearer challenge (the standard Docker Hub flow).
func (c *Client) fetchToken(ctx context.Context, httpc *http.Client, challenge string) (string, error) {
	if !strings.HasPrefix(strings.TrimSpace(challenge), "Bearer ") {
		return "", fmt.Errorf("unsupported auth challenge %q", challenge)
	}
	params := map[string]string{}
	for _, m := range bearerParamRe.FindAllStringSubmatch(challenge, -1) {
		params[m[1]] = m[2]
	}
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("auth challenge without realm")
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if v := params["service"]; v != "" {
		q.Set("service", v)
	}
	if v := params["scope"]; v != "" {
		q.Set("scope", v)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("token endpoint returned HTTP %d", resp.StatusCode)
	}
	var out struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Token != "" {
		return out.Token, nil
	}
	if out.AccessToken != "" {
		return out.AccessToken, nil
	}
	return "", fmt.Errorf("token endpoint returned no token")
}

// PickNewest selects the best replacement tag: the highest version-like tag
// (numeric, semver-aware comparison; a bare release outranks a suffixed
// variant of the same version, e.g. 1.36 > 1.36-musl), excluding the broken
// current tag. When no version-like tag exists, "latest" is used if present.
// Returns "" when there is no usable candidate.
func PickNewest(tags []string, exclude string) string {
	best := ""
	var bestVer version
	hasLatest := false
	for _, tag := range tags {
		if tag == exclude {
			continue
		}
		if tag == "latest" {
			hasLatest = true
			continue
		}
		v, ok := parseVersion(tag)
		if !ok {
			continue
		}
		if best == "" || bestVer.less(v) {
			best, bestVer = tag, v
		}
	}
	if best != "" {
		return best
	}
	if hasLatest && exclude != "latest" {
		return "latest"
	}
	return ""
}

// version is the comparable shape of a version-like tag.
type version struct {
	nums   []int
	suffix string
}

var versionRe = regexp.MustCompile(`^v?(\d+(?:\.\d+)*)(.*)$`)

func parseVersion(tag string) (version, bool) {
	m := versionRe.FindStringSubmatch(tag)
	if m == nil {
		return version{}, false
	}
	parts := strings.Split(m[1], ".")
	nums := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return version{}, false
		}
		nums = append(nums, n)
	}
	return version{nums: nums, suffix: m[2]}, true
}

// less reports whether v orders before other (i.e. other is newer).
func (v version) less(other version) bool {
	n := len(v.nums)
	if len(other.nums) > n {
		n = len(other.nums)
	}
	for i := 0; i < n; i++ {
		a, b := 0, 0
		if i < len(v.nums) {
			a = v.nums[i]
		}
		if i < len(other.nums) {
			b = other.nums[i]
		}
		if a != b {
			return a < b
		}
	}
	// Equal numerics: a bare release ("") outranks any suffixed variant.
	if (v.suffix == "") != (other.suffix == "") {
		return v.suffix != ""
	}
	return v.suffix < other.suffix
}
