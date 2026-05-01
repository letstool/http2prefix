package main

import (
	_ "embed"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/breml/rootcerts" // embed Mozilla CA bundle as fallback for scratch containers

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/oschwald/maxminddb-golang"
)

//go:embed static/index.html
var indexHTML []byte

//go:embed static/favicon.png
var faviconPNG []byte

//go:embed static/openapi.json
var openapiJSON []byte

/* ---------- IPv6 MMDB keyspace for AS→Prefix ---------- */

// as2prefixIPv6Base is the fixed 10-byte ULA prefix for the synthetic AS→IPv6 keyspace.
// Identical to db2prefix (fdac:db01::/80) for full MMDB format compatibility.
//
// The full 16-byte IPv6 MMDB key is:
//
//	bytes  0–9  : fd:ac:db:01:00:00:00:00:00:00   (this prefix, 80 bits)
//	bytes 10–13 : ASN as big-endian uint32
//	bytes 14–15 : 0x00, 0x00 (zero)
//	mask         : /128 (one unique address per ASN, no subnet)
//
// Example: AS 13335 (0x00003417) → fdac:db01::0:3417:0 /128
//
// fdac:db01::/32 is within the ULA range (fc00::/7) and never routes publicly.
var as2prefixIPv6Base = [10]byte{0xfd, 0xac, 0xdb, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}

/* ---------- MMDB record types ---------- */

// PrefixASRecord is the data stored per IP prefix in the prefix2as MMDB.
// A prefix may be originated by multiple autonomous systems (MOAS).
type PrefixASRecord struct {
	Prefix  string   `maxminddb:"prefix"`
	Origins []uint32 `maxminddb:"origins"` // one or more originating AS numbers
}

// ASPrefixRecord is the data stored per AS number in the as2prefix MMDB.
// Prefixes are stored sorted: IPv4 ascending, then IPv6 ascending.
// Peers is the merged, deduplicated, ascending-sorted union of all BGP
// neighbours (peers, upstreams, and downstreams) as classified by db2prefix.
type ASPrefixRecord struct {
	ASNum    uint32   `maxminddb:"asn"`      // db2prefix compat: MMDB field is "asn"
	Prefixes []string `maxminddb:"prefixes"`
	Peers    []uint32 `maxminddb:"peers"`    // merged union of peers + upstreams + downstreams, sorted ascending
}

/* ---------- API types ---------- */

// LookupRequest is the JSON body expected by the /api/v1/asprefix endpoint.
type LookupRequest struct {
	Query *string `json:"query"` // IP address or ASNUM (e.g. "AS65000")
}

// IPAnswer is the answer for an IP address lookup (prefix2as).
// Origins lists all ASNs announcing the matched prefix (MOAS support).
type IPAnswer struct {
	IP      string   `json:"ip"`
	Prefix  string   `json:"prefix,omitempty"`
	Size    string   `json:"size,omitempty"`    // address count in the matched prefix
	Origins []uint32 `json:"origins,omitempty"` // originating AS numbers
}

// PrefixInfo pairs a CIDR prefix with its address count.
// Size is a decimal string for IPv4 (e.g. "256") and a power-of-2 string
// for IPv6 (e.g. "2^96") since IPv6 block sizes cannot fit in a uint64.
type PrefixInfo struct {
	Prefix string `json:"prefix"`
	Size   string `json:"size"`
}

// ASAnswer is the answer for an AS number lookup (as2prefix).
// Prefixes are sorted: IPv4 addresses ascending, then IPv6 addresses ascending.
// Peers is the merged, deduplicated, ascending-sorted union of all BGP
// neighbours (peers, upstreams, and downstreams) as classified by db2prefix.
type ASAnswer struct {
	ASNum    uint32       `json:"asnum"`
	Prefixes []PrefixInfo `json:"prefixes"`
	Peers    []uint32     `json:"peers,omitempty"` // merged union of peers + upstreams + downstreams, sorted ascending
}

// LookupResponse is the JSON body returned by the /api/v1/asprefix endpoint.
type LookupResponse struct {
	Status    string      `json:"status"`              // SUCCESS, NOTFOUND, ERROR
	QueryType string      `json:"query_type"`          // "ip" or "asnum"
	Query     string      `json:"query"`
	Answer    interface{} `json:"answer,omitempty"`
}

/* ---------- Configuration ---------- */

var (
	dbDir      string
	dbURL      string // base URL of a peer instance; empty = build from CDN CSV
	licenseKey string // LICENSE_KEY token for the CDN; may be empty (anonymous)
	listenAddr string

	prefix2asDB atomic.Value // *maxminddb.Reader
	as2prefixDB atomic.Value // *maxminddb.Reader
)

const (
	// prefix2as DB
	prefix2asUpdateFile   = ".last_update_prefix2as"
	prefix2asModifiedFile = ".last_modified_prefix2as"
	prefix2asDBFileName   = "prefix2as.mmdb"
	cdnPrefix2asURL       = "https://cdn.letstool.net/prefix2as/csv"

	// as2prefix DB
	as2prefixUpdateFile   = ".last_update_as2prefix"
	as2prefixModifiedFile = ".last_modified_as2prefix"
	as2prefixDBFileName   = "as2prefix.mmdb"
	cdnAs2prefixURL       = "https://cdn.letstool.net/as2prefix/csv"
)

// asnumRe matches AS numbers in any case: AS123, as123, As123
var asnumRe = regexp.MustCompile(`(?i)^as([0-9]+)$`)

/* ---------- Query detection ---------- */

// parseASNum parses "AS12345" (case-insensitive) and returns the numeric ASN.
// Returns 0 and false if the input is not a valid ASNUM.
func parseASNum(s string) (uint32, bool) {
	m := asnumRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseUint(m[1], 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(n), true
}

// detectQueryType returns "ip", "asnum", or "" for unknown.
func detectQueryType(q string) string {
	q = strings.TrimSpace(q)
	if ip := net.ParseIP(q); ip != nil {
		return "ip"
	}
	if _, ok := parseASNum(q); ok {
		return "asnum"
	}
	return ""
}

/* ---------- AS IPv6 key helpers ---------- */

// asnToIPv6 builds the synthetic /128 IPv6 lookup key for an ASN.
// Layout: as2prefixIPv6Base (80 bits) | ASN uint32 big-endian (32 bits) | 0x0000 (16 bits).
// Matches the db2prefix keyspace exactly.
func asnToIPv6(asn uint32) net.IP {
	ip := make(net.IP, 16)
	copy(ip[0:10], as2prefixIPv6Base[:])
	ip[10] = byte(asn >> 24)
	ip[11] = byte(asn >> 16)
	ip[12] = byte(asn >> 8)
	ip[13] = byte(asn)
	// bytes 14-15 remain zero
	return ip
}

// asnToNetwork returns the /128 network used to INSERT an AS record in the MMDB.
// Each ASN occupies exactly one unique address — matches db2prefix.
func asnToNetwork(asn uint32) *net.IPNet {
	ip := asnToIPv6(asn)
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}
}

/* ---------- Helpers ---------- */

func writeTimestamp(file string) {
	p := filepath.Join(dbDir, file)
	if err := os.WriteFile(p, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0644); err != nil {
		log.Printf("Warning: could not write %s: %v", file, err)
	}
}

func readLastUpdate(file string) time.Time {
	data, err := os.ReadFile(filepath.Join(dbDir, file))
	if err != nil {
		return time.Time{}
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(ts, 0)
}

// lastCDNSlot returns the most recent 12:00:00 UTC that has already passed.
// This is the timestamp at which the CDN last published fresh BGP data.
func lastCDNSlot(now time.Time) time.Time {
	now = now.UTC()
	slot := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	if now.Before(slot) {
		// Today's slot hasn't happened yet — use yesterday's.
		slot = slot.AddDate(0, 0, -1)
	}
	return slot
}

// nextCDNSlot returns the next 12:00:00 UTC strictly after now.
func nextCDNSlot(now time.Time) time.Time {
	now = now.UTC()
	slot := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	if !now.Before(slot) {
		slot = slot.AddDate(0, 0, 1)
	}
	return slot
}

// nextCDNSlotDuration returns the duration until the next CDN publication slot (12:00 UTC).
func nextCDNSlotDuration() time.Duration {
	return time.Until(nextCDNSlot(time.Now()))
}

// wasUpdatedSinceLastCDNSlot returns true if the timestamp file records a
// successful update that happened at or after the last CDN publication slot
// (12:00 UTC). A true result means the local DB already reflects the latest
// CDN data and no new fetch is needed until the next slot.
func wasUpdatedSinceLastCDNSlot(file string) bool {
	t := readLastUpdate(file)
	if t.IsZero() {
		return false
	}
	return !t.Before(lastCDNSlot(time.Now()))
}

func readLastModified(file string) string {
	data, err := os.ReadFile(filepath.Join(dbDir, file))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func writeLastModified(file, value string) {
	if value == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dbDir, file), []byte(value), 0644); err != nil {
		log.Printf("Warning: could not write %s: %v", file, err)
	}
}

func swapDB(store *atomic.Value, newDB *maxminddb.Reader) {
	old := store.Swap(newDB)
	if old != nil {
		if r, ok := old.(*maxminddb.Reader); ok {
			r.Close()
		}
	}
}

func installFile(src, dst string) error {
	_ = os.Remove(dst)
	if err := os.Rename(src, dst); err != nil {
		return copyFile(src, dst)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

/* ---------- HTTP client ---------- */

func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

func logProxyConfig(targetURL string) {
	u, err := url.Parse(targetURL)
	if err != nil {
		return
	}
	req := &http.Request{URL: u}
	proxyURL, err := http.ProxyFromEnvironment(req)
	if err != nil || proxyURL == nil {
		log.Printf("Proxy: none (direct connection to %s)", u.Host)
		return
	}
	safe := *proxyURL
	if safe.User != nil {
		if _, hasPwd := safe.User.Password(); hasPwd {
			safe.User = url.UserPassword(safe.User.Username(), "***")
		}
	}
	log.Printf("Proxy: %s (for %s)", safe.String(), u.Host)
}

/* ---------- CDN error types ---------- */

var errNotModified = errors.New("CSV not modified (304)")

type errRateLimited struct {
	RetryAfter int64
}

func (e *errRateLimited) Error() string {
	return fmt.Sprintf("CDN rate-limited (429) — retry after unix timestamp %d (%s)",
		e.RetryAfter,
		time.Unix(e.RetryAfter, 0).UTC().Format(time.RFC3339))
}

type errProductGone struct {
	Body string
}

func (e *errProductGone) Error() string {
	return fmt.Sprintf("CDN product gone (410): %s", e.Body)
}

type errUnauthorized struct {
	Message string
}

func (e *errUnauthorized) Error() string {
	return fmt.Sprintf("CDN unauthorized (401): %s", e.Message)
}

func extractJSONMessage(body []byte) string {
	var obj struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &obj); err == nil && obj.Message != "" {
		return obj.Message
	}
	return strings.TrimSpace(string(body))
}

// gzipReadCloser wraps a gzip.Reader and its underlying HTTP response body.
type gzipReadCloser struct {
	gz   *gzip.Reader
	body io.ReadCloser
}

func (g *gzipReadCloser) Read(p []byte) (int, error) { return g.gz.Read(p) }
func (g *gzipReadCloser) Close() error {
	err1 := g.gz.Close()
	err2 := g.body.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

/* ---------- CDN fetch helper ---------- */

// fetchCSVFromCDN fetches the gzipped CSV from the given CDN URL.
// Handles 304, 429, 410, and 401 per the letstool CDN protocol.
func fetchCSVFromCDN(ctx context.Context, cdnURL, modifiedFile string) (io.ReadCloser, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cdnURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create CDN request: %w", err)
	}

	req.Header.Set("User-Agent", "http2prefix/1.0 (+https://github.com/letstool/http2prefix)")

	if licenseKey != "" {
		req.Header.Set("Authorization", "Basic "+licenseKey)
	}

	if lm := readLastModified(modifiedFile); lm != "" {
		req.Header.Set("If-Modified-Since", lm)
		log.Printf("[%s] CDN request with If-Modified-Since: %s", cdnURL, lm)
	}

	client := newHTTPClient(180 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("CDN GET %s: %w", cdnURL, err)
	}

	switch resp.StatusCode {
	case http.StatusNotModified:
		resp.Body.Close()
		log.Printf("[%s] CDN: CSV not modified (304) — current DB is up to date", cdnURL)
		return nil, "", errNotModified

	case http.StatusTooManyRequests:
		ra := resp.Header.Get("Retry-After")
		resp.Body.Close()
		ts, _ := strconv.ParseInt(strings.TrimSpace(ra), 10, 64)
		return nil, "", &errRateLimited{RetryAfter: ts}

	case http.StatusGone:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, "", &errProductGone{Body: strings.TrimSpace(string(body))}

	case http.StatusUnauthorized:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, "", &errUnauthorized{Message: extractJSONMessage(body)}

	case http.StatusOK:

	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		resp.Body.Close()
		return nil, "", fmt.Errorf("CDN returned %s: %s", resp.Status, body)
	}

	lastModified := resp.Header.Get("Last-Modified")

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		resp.Body.Close()
		return nil, "", fmt.Errorf("CDN gzip reader: %w", err)
	}

	return &gzipReadCloser{gz: gz, body: resp.Body}, lastModified, nil
}

/* ---------- prefix2as DB build ---------- */

// prefixRow holds a parsed row from the prefix2as CSV, ready for insertion.
type prefixRow struct {
	network    *net.IPNet
	prefix     string
	origins    []uint32 // one or more originating ASNs
	prefixBits int      // used for sorting (shorter = more general = insert first)
	isIPv4     bool
}

// sortPrefixes sorts a list of CIDR prefix strings:
// IPv4 addresses ascending (by network address then prefix length),
// followed by IPv6 addresses ascending (by network address then prefix length).
// Invalid strings are appended last, unchanged.
func sortPrefixes(prefixes []string) []string {
	type entry struct {
		raw  string
		netIP net.IP // masked network address
		bits  int
		isV4  bool
		valid bool
	}

	entries := make([]entry, 0, len(prefixes))
	for _, p := range prefixes {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		_, ipnet, err := net.ParseCIDR(p)
		if err != nil {
			entries = append(entries, entry{raw: p})
			continue
		}
		bits, _ := ipnet.Mask.Size()
		isV4 := ipnet.IP.To4() != nil
		var netIP net.IP
		if isV4 {
			netIP = ipnet.IP.To4()
		} else {
			netIP = ipnet.IP.To16()
		}
		entries = append(entries, entry{raw: p, netIP: netIP, bits: bits, isV4: isV4, valid: true})
	}

	sort.SliceStable(entries, func(i, j int) bool {
		// Invalid entries sink to the end.
		if !entries[i].valid && !entries[j].valid {
			return false
		}
		if !entries[i].valid {
			return false
		}
		if !entries[j].valid {
			return true
		}
		// IPv4 before IPv6.
		if entries[i].isV4 != entries[j].isV4 {
			return entries[i].isV4
		}
		// Within the same family: compare network addresses byte-by-byte.
		cmp := bytes.Compare(entries[i].netIP, entries[j].netIP)
		if cmp != 0 {
			return cmp < 0
		}
		// Same network address: shorter prefix length first.
		return entries[i].bits < entries[j].bits
	})

	result := make([]string, 0, len(entries))
	for _, e := range entries {
		result = append(result, e.raw)
	}
	return result
}

// buildPrefix2asDBFromCSV fetches the prefix2as CSV from the CDN, parses it,
// sorts prefixes (parents before children to ensure correct MMDB layering),
// and compiles a fresh prefix2as.mmdb.
//
// CSV format (header row required):
//
//	prefix,origins
//
// The "origins" column contains one or more space-separated AS numbers.
// A prefix announced by multiple ASes (MOAS) will have multiple values.
// Example: "1.0.0.0/24,13335" or "1.2.3.0/24,65001 65002"
func buildPrefix2asDBFromCSV(ctx context.Context) error {
	log.Printf("Fetching prefix2as CSV from CDN: %s", cdnPrefix2asURL)

	csvReader, lastModified, err := fetchCSVFromCDN(ctx, cdnPrefix2asURL, prefix2asModifiedFile)
	if err != nil {
		if err == errNotModified {
			writeTimestamp(prefix2asUpdateFile)
			return nil
		}
		return fmt.Errorf("prefix2as CDN fetch: %w", err)
	}
	defer csvReader.Close()

	writer, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType:            "db2prefix-PREFIX2AS", // matches db2prefix for interoperability
		Description:             map[string]string{"en": "BGP prefix → origin ASes (built by http2prefix)"},
		RecordSize:              28,
		IPVersion:               6,
		IncludeReservedNetworks: true,
	})
	if err != nil {
		return fmt.Errorf("create prefix2as mmdb writer: %w", err)
	}

	r := csv.NewReader(csvReader)
	r.ReuseRecord = true
	r.FieldsPerRecord = 2 // exactly: prefix, origins

	// Read and parse header.
	header, err := r.Read()
	if err != nil {
		return fmt.Errorf("read prefix2as CSV header: %w", err)
	}
	colIdx := make(map[string]int)
	for i, h := range header {
		colIdx[strings.ToLower(strings.TrimSpace(h))] = i
	}

	colPrefix, hasPrefix := colIdx["prefix"]
	colOrigins, hasOrigins := colIdx["origins"]
	if !hasPrefix || !hasOrigins {
		return fmt.Errorf("prefix2as CSV missing required columns 'prefix' and/or 'origins' (found: %v)", header)
	}

	// Collect all rows so we can sort by prefix length before insertion.
	var rows []prefixRow
	lineNum := 1

	for {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("Warning: prefix2as CSV parse error at line %d: %v — skipping", lineNum+1, err)
			lineNum++
			continue
		}
		lineNum++

		if len(record) <= colPrefix || len(record) <= colOrigins {
			continue
		}

		pfxStr := strings.TrimSpace(record[colPrefix])

		// Parse network.
		_, network, err := net.ParseCIDR(pfxStr)
		if err != nil {
			log.Printf("Warning: prefix2as invalid prefix %q at line %d — skipping", pfxStr, lineNum)
			continue
		}

		// Parse origins: space-separated list of AS numbers (numeric or "AS12345").
		originParts := strings.Fields(record[colOrigins])
		if len(originParts) == 0 {
			continue
		}
		var origins []uint32
		for _, part := range originParts {
			part = strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(part)), "AS")
			n, err := strconv.ParseUint(part, 10, 32)
			if err != nil {
				log.Printf("Warning: prefix2as invalid ASN %q at line %d — skipping ASN", part, lineNum)
				continue
			}
			origins = append(origins, uint32(n))
		}
		if len(origins) == 0 {
			continue
		}

		prefixOnes, _ := network.Mask.Size()
		isv4 := network.IP.To4() != nil

		rows = append(rows, prefixRow{
			network:    network,
			prefix:     network.String(),
			origins:    origins,
			prefixBits: prefixOnes,
			isIPv4:     isv4,
		})
	}

	// Sort by prefix length ascending: parents (shorter) before children (longer).
	// CRITICAL for correct MMDB trie layering.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].isIPv4 != rows[j].isIPv4 {
			return rows[i].isIPv4 // v4 before v6 (different subtrees)
		}
		return rows[i].prefixBits < rows[j].prefixBits
	})

	inserted := 0
	for _, row := range rows {
		originsSlice := make(mmdbtype.Slice, len(row.origins))
		for i, asn := range row.origins {
			originsSlice[i] = mmdbtype.Uint32(asn)
		}
		// Use 4-byte form for IPv4 so mmdbwriter maps it into the standard
		// IPv4-in-IPv6 subtree (::ffff:0:0/96) — matches db2prefix behaviour.
		ipNet := row.network
		if ip4 := ipNet.IP.To4(); ip4 != nil {
			ipNet = &net.IPNet{IP: ip4, Mask: ipNet.Mask}
		}
		rec := mmdbtype.Map{
			"prefix":  mmdbtype.String(row.prefix),
			"origins": originsSlice,
		}
		if err := writer.Insert(ipNet, rec); err != nil {
			log.Printf("Warning: prefix2as failed to insert %s (line ~%d): %v", row.prefix, inserted+2, err)
			continue
		}
		inserted++
	}
	log.Printf("prefix2as: inserted %d prefix records from CDN CSV", inserted)

	return finalizeMmdb(writer, prefix2asDBFileName, prefix2asUpdateFile, prefix2asModifiedFile, lastModified, &prefix2asDB)
}

/* ---------- as2prefix DB build ---------- */

// buildAs2prefixDBFromCSV fetches the as2prefix CSV from the CDN and compiles
// a fresh as2prefix.mmdb using a synthetic ULA IPv6 keyspace.
//
// CSV format (header row required):
//
//	asn,prefixes,peers,upstreams,downstreams
//
// The "prefixes" column contains all prefixes for that ASN as a space-separated
// list on a single row. The "peers", "upstreams", and "downstreams" columns each
// contain space-separated AS numbers representing BGP neighbour relationships as
// classified by db2prefix.
//
// All three neighbour columns are merged into a single deduplicated, ascending-sorted
// list stored in the MMDB as "peers". This union represents the full set of BGP
// neighbours regardless of their directionality.
//
// Prefixes are sorted before storage: IPv4 ascending by network address, then IPv6
// ascending by network address.
//
// Example: "13335,1.1.1.0/24 1.0.0.0/24 2606:4700::/32,1234 5678,9000,7777"
func buildAs2prefixDBFromCSV(ctx context.Context) error {
	log.Printf("Fetching as2prefix CSV from CDN: %s", cdnAs2prefixURL)

	csvReader, lastModified, err := fetchCSVFromCDN(ctx, cdnAs2prefixURL, as2prefixModifiedFile)
	if err != nil {
		if err == errNotModified {
			writeTimestamp(as2prefixUpdateFile)
			return nil
		}
		return fmt.Errorf("as2prefix CDN fetch: %w", err)
	}
	defer csvReader.Close()

	r := csv.NewReader(csvReader)
	r.ReuseRecord = true
	r.FieldsPerRecord = 5 // exactly: asn, prefixes, peers, upstreams, downstreams

	header, err := r.Read()
	if err != nil {
		return fmt.Errorf("read as2prefix CSV header: %w", err)
	}
	colIdx := make(map[string]int)
	for i, h := range header {
		colIdx[strings.ToLower(strings.TrimSpace(h))] = i
	}

	colASN, hasASN := colIdx["asn"]
	colPrefixes, hasPrefixes := colIdx["prefixes"]
	colPeers := colIdx["peers"]
	colUpstreams := colIdx["upstreams"]
	colDownstreams := colIdx["downstreams"]
	if !hasASN || !hasPrefixes {
		return fmt.Errorf("as2prefix CSV missing required columns 'asn' and/or 'prefixes' (found: %v)", header)
	}

	writer, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType:            "db2prefix-AS2PREFIX",  // matches db2prefix for interoperability
		Description:             map[string]string{"en": "ASN → announced BGP prefixes (built by http2prefix)"},
		RecordSize:              28,
		IPVersion:               6,
		IncludeReservedNetworks: true, // fdac:db01::/32 is ULA
	})
	if err != nil {
		return fmt.Errorf("create as2prefix mmdb writer: %w", err)
	}

	inserted := 0
	totalPrefixes := 0
	lineNum := 1

	for {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("Warning: as2prefix CSV parse error at line %d: %v — skipping", lineNum+1, err)
			lineNum++
			continue
		}
		lineNum++

		if len(record) <= colASN || len(record) <= colPrefixes {
			continue
		}

		// Parse ASN.
		asnStr := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(record[colASN])), "AS")
		asnVal, err := strconv.ParseUint(asnStr, 10, 32)
		if err != nil {
			log.Printf("Warning: as2prefix invalid ASN %q at line %d — skipping", record[colASN], lineNum)
			continue
		}
		asn := uint32(asnVal)

		// Parse prefixes: space-separated list on a single field.
		pfxParts := strings.Fields(record[colPrefixes])
		if len(pfxParts) == 0 {
			continue
		}

		// Validate and collect prefixes.
		var validPrefixes []string
		for _, p := range pfxParts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			_, _, err := net.ParseCIDR(p)
			if err != nil {
				log.Printf("Warning: as2prefix invalid prefix %q for AS%d at line %d — skipping", p, asn, lineNum)
				continue
			}
			validPrefixes = append(validPrefixes, p)
		}
		if len(validPrefixes) == 0 {
			continue
		}

		// Sort: IPv4 ascending by network address, then IPv6 ascending by network address.
		sortedPrefixes := sortPrefixes(validPrefixes)
		totalPrefixes += len(sortedPrefixes)

		// Merge peers + upstreams + downstreams into one deduplicated, ascending-sorted set.
		// All three columns represent BGP neighbours; directionality is discarded and the
		// union is exposed as a single "peers" list in the API and MMDB.
		asnSet := make(map[uint32]struct{})
		for _, col := range []int{colPeers, colUpstreams, colDownstreams} {
			if col >= len(record) {
				continue
			}
			for _, part := range strings.Fields(record[col]) {
				part = strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(part)), "AS")
				if part == "" {
					continue
				}
				n, err := strconv.ParseUint(part, 10, 32)
				if err != nil {
					log.Printf("Warning: as2prefix invalid neighbour ASN %q for AS%d at line %d — skipping", part, asn, lineNum)
					continue
				}
				asnSet[uint32(n)] = struct{}{}
			}
		}
		sortedPeers := make([]uint32, 0, len(asnSet))
		for a := range asnSet {
			sortedPeers = append(sortedPeers, a)
		}
		sort.Slice(sortedPeers, func(i, j int) bool { return sortedPeers[i] < sortedPeers[j] })

		network := asnToNetwork(asn)
		pfxSlice := make(mmdbtype.Slice, len(sortedPrefixes))
		for i, p := range sortedPrefixes {
			pfxSlice[i] = mmdbtype.String(p)
		}
		peersSlice := make(mmdbtype.Slice, len(sortedPeers))
		for i, p := range sortedPeers {
			peersSlice[i] = mmdbtype.Uint32(p)
		}

		// MMDB field "asn" matches db2prefix format (not "asnum").
		rec := mmdbtype.Map{
			"asn":      mmdbtype.Uint32(asn),
			"prefixes": pfxSlice,
			"peers":    peersSlice,
		}
		if err := writer.Insert(network, rec); err != nil {
			log.Printf("Warning: as2prefix failed to insert AS%d: %v", asn, err)
			continue
		}
		inserted++
	}
	log.Printf("as2prefix: inserted %d AS records (%d prefixes total) from CDN CSV", inserted, totalPrefixes)

	return finalizeMmdb(writer, as2prefixDBFileName, as2prefixUpdateFile, as2prefixModifiedFile, lastModified, &as2prefixDB)
}

/* ---------- finalizeMmdb (shared) ---------- */

// finalizeMmdb writes the MMDB to a temp file, atomically swaps it into the given
// store, persists it to the final path, and updates the marker files.
func finalizeMmdb(
	writer *mmdbwriter.Tree,
	dbFileName, updateFile, modifiedFile, lastModified string,
	store *atomic.Value,
) error {
	tmpFile, err := os.CreateTemp(dbDir, "build-*.mmdb")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmpFile.Name()
	defer os.Remove(tmpName)

	if _, err := writer.WriteTo(tmpFile); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write mmdb: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	newDB, err := maxminddb.Open(tmpName)
	if err != nil {
		return fmt.Errorf("open new mmdb: %w", err)
	}

	swapDB(store, newDB)

	finalPath := filepath.Join(dbDir, dbFileName)
	if err := installFile(tmpName, finalPath); err != nil {
		return fmt.Errorf("install mmdb: %w", err)
	}

	writeTimestamp(updateFile)
	writeLastModified(modifiedFile, lastModified)
	log.Printf("%s built and loaded successfully", dbFileName)
	return nil
}

/* ---------- Peer download mode ---------- */

// downloadFromPeer fetches one mmdb file from the given peer endpoint path,
// atomically swaps it into store, and persists it to dbFileName.
func downloadFromPeer(ctx context.Context, peerEndpointPath, dbFileName, updateFile string, store *atomic.Value) error {
	u, err := url.Parse(dbURL)
	if err != nil {
		return fmt.Errorf("invalid DB_URL %q: %w", dbURL, err)
	}
	u.Path = peerEndpointPath
	peerURL := u.String()
	log.Printf("Downloading %s from peer: %s", dbFileName, peerURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, peerURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	client := newHTTPClient(120 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("peer GET %s: %w", peerURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("peer returned %s", resp.Status)
	}

	tmpFile, err := os.CreateTemp(dbDir, "peer-*.mmdb")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmpFile.Name()
	defer os.Remove(tmpName)

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write peer mmdb: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	newDB, err := maxminddb.Open(tmpName)
	if err != nil {
		return fmt.Errorf("open peer mmdb: %w", err)
	}

	swapDB(store, newDB)

	finalPath := filepath.Join(dbDir, dbFileName)
	if err := installFile(tmpName, finalPath); err != nil {
		log.Printf("Warning: could not persist peer mmdb %s: %v", dbFileName, err)
	}

	writeTimestamp(updateFile)
	log.Printf("Peer mmdb download complete: %s", dbFileName)
	return nil
}

/* ---------- Update dispatch ---------- */

// updatePrefix2as runs the right update strategy for the prefix2as database.
func updatePrefix2as(ctx context.Context) error {
	if dbURL != "" {
		return downloadFromPeer(ctx, "/db/prefix2as", prefix2asDBFileName, prefix2asUpdateFile, &prefix2asDB)
	}
	return buildPrefix2asDBFromCSV(ctx)
}

// updateAs2prefix runs the right update strategy for the as2prefix database.
func updateAs2prefix(ctx context.Context) error {
	if dbURL != "" {
		return downloadFromPeer(ctx, "/db/as2prefix", as2prefixDBFileName, as2prefixUpdateFile, &as2prefixDB)
	}
	return buildAs2prefixDBFromCSV(ctx)
}

// loadOrBuild checks if a DB file is already on disk and fresh (updated since the last
// CDN publication slot at 12:00 UTC); if so, loads it. Otherwise calls the builder.
func loadOrBuild(
	ctx context.Context,
	dbFileName, updateFile string,
	store *atomic.Value,
	builder func(context.Context) error,
) error {
	mmdbPath := filepath.Join(dbDir, dbFileName)
	if _, err := os.Stat(mmdbPath); err == nil {
		// Always load the existing file into memory first.
		// This ensures the store is never nil when the CDN returns 304 Not Modified
		// (the builder returns without calling swapDB in that case).
		// If the builder later produces a fresher database, swapDB() will replace it.
		if db, openErr := maxminddb.Open(mmdbPath); openErr == nil {
			store.Store(db)
			t := readLastUpdate(updateFile)
			log.Printf("Loaded existing %s into memory (last built: %s)", dbFileName, t.Format(time.RFC3339))
		} else {
			log.Printf("Warning: existing %s is unreadable (%v) — will rebuild", dbFileName, openErr)
		}

		// If the DB was updated after the last CDN slot (12:00 UTC), it already
		// reflects the latest data — skip the CDN call until the next slot.
		if wasUpdatedSinceLastCDNSlot(updateFile) && store.Load() != nil {
			log.Printf("%s is current (updated since last CDN slot at 12:00 UTC) — skipping CDN check", dbFileName)
			return nil
		}
		log.Printf("%s: checking CDN for updates (existing DB remains active in memory)", dbFileName)
	}

	// Either no file on disk, or file may be stale.
	// Call the builder; if CDN returns 304 the in-memory DB (loaded above) is still served.
	// If the builder produces a new database it will hot-swap via swapDB().
	return builder(ctx)
}

/* ---------- Scheduler ---------- */

var goneRetrySchedule = []time.Duration{
	24 * time.Hour,
	48 * time.Hour,
	72 * time.Hour,
	96 * time.Hour,
}

// schedulePeriodicUpdate fires updateFn at the next CDN publication slot (12:00 UTC),
// then every 24 hours aligned to that slot.
// Handles 429, 410, and 401 CDN responses with appropriate back-off or stop.
func schedulePeriodicUpdate(ctx context.Context, name string, updateFn func(context.Context) error) {
	go func() {
		goneAttempt := 0

		delay := nextCDNSlotDuration()
		log.Printf("[%s] Next scheduled update in %s (at 12:00 UTC)", name, delay.Round(time.Minute))
		timer := time.NewTimer(delay)
		defer timer.Stop()

		for {
			select {
			case <-timer.C:
				err := updateFn(ctx)

				if err == nil {
					if goneAttempt > 0 {
						log.Printf("[%s] Update succeeded after %d gone-retry attempt(s)", name, goneAttempt)
						goneAttempt = 0
					}
					next := nextCDNSlotDuration()
					log.Printf("[%s] Next update in %s (at 12:00 UTC)", name, next.Round(time.Minute))
					timer.Reset(next)
					continue
				}

				var rl *errRateLimited
				if errors.As(err, &rl) && rl.RetryAfter > 0 {
					wait := time.Until(time.Unix(rl.RetryAfter, 0))
					if wait <= 0 {
						wait = 24 * time.Hour
					}
					log.Printf("[%s] Rate-limited by CDN: next attempt in %s", name, wait.Round(time.Second))
					timer.Reset(wait)
					continue
				}

				var gone *errProductGone
				if errors.As(err, &gone) {
					if goneAttempt >= len(goneRetrySchedule) {
						log.Printf("[%s] CDN [410] All retry attempts exhausted — stopping permanently.", name)
						if gone.Body != "" {
							log.Printf("[%s] CDN [410] Server message: %s", name, gone.Body)
						}
						return
					}
					wait := goneRetrySchedule[goneAttempt]
					log.Printf("[%s] CDN [410] Product gone (attempt %d/%d) — retry in %s", name, goneAttempt+1, len(goneRetrySchedule), wait)
					goneAttempt++
					timer.Reset(wait)
					continue
				}

				var unauth *errUnauthorized
				if errors.As(err, &unauth) {
					log.Printf("[%s] CDN [401] Authorization refused — stopping permanently.", name)
					log.Printf("[%s] CDN [401] Server message: %s", name, unauth.Message)
					return
				}

				// General error: retry at the next CDN slot.
				next := nextCDNSlotDuration()
				log.Printf("[%s] Update failed: %v — retrying in %s (at 12:00 UTC)", name, err, next.Round(time.Minute))
				timer.Reset(next)

			case <-ctx.Done():
				return
			}
		}
	}()
}

/* ---------- Lookup ---------- */

// prefixSize returns a human-readable address count for a CIDR prefix.
// IPv4 prefixes: exact decimal number (max 2^32, fits in uint64).
// IPv6 prefixes: power-of-two notation ("2^N") since block sizes can exceed uint64.
// Special cases: /32 IPv4 or /128 IPv6 → "1"; unknown → "".
func prefixSize(cidr string) string {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}
	ones, bits := ipnet.Mask.Size()
	hostBits := bits - ones
	if hostBits == 0 {
		return "1"
	}
	if bits == 32 { // IPv4: compute exact decimal, 2^hostBits ≤ 2^32 fits in uint64
		return strconv.FormatUint(uint64(1)<<uint(hostBits), 10)
	}
	// IPv6: power-of-two notation
	return fmt.Sprintf("2^%d", hostBits)
}

// lookupIP queries the prefix2as MMDB for an IP address.
// Returns the matched prefix and all its originating AS numbers.
func lookupIP(db *maxminddb.Reader, ipStr string) (*IPAnswer, bool) {
	ip := net.ParseIP(strings.TrimSpace(ipStr))
	if ip == nil {
		return nil, false
	}

	// Use 16-byte form for MMDB lookup (IPv4-mapped IPv6 for v4 addresses).
	ip16 := ip.To16()
	if ip16 == nil {
		return nil, false
	}

	var rec PrefixASRecord
	if err := db.Lookup(ip16, &rec); err != nil {
		log.Printf("prefix2as lookup error for %s: %v", ipStr, err)
		return nil, false
	}

	ans := &IPAnswer{IP: ip.String()}
	if len(rec.Origins) == 0 {
		return ans, false
	}
	ans.Prefix  = rec.Prefix
	ans.Size    = prefixSize(rec.Prefix)
	ans.Origins = rec.Origins
	return ans, true
}

// lookupASN queries the as2prefix MMDB for an AS number.
// Returns the AS number and its sorted list of announced prefixes.
func lookupASN(db *maxminddb.Reader, query string) (*ASAnswer, bool) {
	asn, ok := parseASNum(query)
	if !ok {
		return nil, false
	}

	lookupKey := asnToIPv6(asn)

	var rec ASPrefixRecord
	if err := db.Lookup(lookupKey, &rec); err != nil {
		log.Printf("as2prefix lookup error for AS%d: %v", asn, err)
		return nil, false
	}

	if rec.ASNum == 0 {
		return &ASAnswer{ASNum: asn, Prefixes: []PrefixInfo{}}, false
	}
	pfxInfos := make([]PrefixInfo, len(rec.Prefixes))
	for i, p := range rec.Prefixes {
		pfxInfos[i] = PrefixInfo{Prefix: p, Size: prefixSize(p)}
	}
	return &ASAnswer{
		ASNum:    rec.ASNum,
		Prefixes: pfxInfos,
		Peers:    rec.Peers,
	}, true
}

/* ---------- HTTP Handlers ---------- */

func indexHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func faviconHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Write(faviconPNG)
}

func openapiHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(openapiJSON)
}

func respondLookup(w http.ResponseWriter, resp LookupResponse) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func lookupHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Only POST allowed", http.StatusMethodNotAllowed)
			return
		}

		var req LookupRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Query == nil {
			respondLookup(w, LookupResponse{Status: "ERROR", Query: ""})
			return
		}
		defer r.Body.Close()

		query := strings.TrimSpace(*req.Query)
		if query == "" {
			respondLookup(w, LookupResponse{Status: "ERROR", Query: query})
			return
		}

		qtype := detectQueryType(query)

		switch qtype {
		case "ip":
			dbVal := prefix2asDB.Load()
			if dbVal == nil {
				respondLookup(w, LookupResponse{Status: "ERROR", QueryType: "ip", Query: query})
				return
			}
			db := dbVal.(*maxminddb.Reader)
			ans, found := lookupIP(db, query)
			if !found {
				respondLookup(w, LookupResponse{Status: "NOTFOUND", QueryType: "ip", Query: query, Answer: ans})
				return
			}
			respondLookup(w, LookupResponse{Status: "SUCCESS", QueryType: "ip", Query: query, Answer: ans})

		case "asnum":
			dbVal := as2prefixDB.Load()
			if dbVal == nil {
				respondLookup(w, LookupResponse{Status: "ERROR", QueryType: "asnum", Query: query})
				return
			}
			db := dbVal.(*maxminddb.Reader)
			ans, found := lookupASN(db, query)
			if !found {
				respondLookup(w, LookupResponse{Status: "NOTFOUND", QueryType: "asnum", Query: query, Answer: ans})
				return
			}
			respondLookup(w, LookupResponse{Status: "SUCCESS", QueryType: "asnum", Query: query, Answer: ans})

		default:
			respondLookup(w, LookupResponse{Status: "ERROR", QueryType: "unknown", Query: query})
		}
	}
}

// getDBHandler serves the given mmdb file for peer synchronisation.
func getDBHandler(fileName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mmdbPath := filepath.Join(dbDir, fileName)
		if _, err := os.Stat(mmdbPath); err != nil {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}
		http.ServeFile(w, r, mmdbPath)
	}
}

/* ---------- Main ---------- */

func main() {
	const sentinel = "\x00"
	flagDBURL      := flag.String("db-url",      sentinel, "Base URL of a peer http2prefix instance (e.g. http://host:8080). Overrides DB_URL.")
	flagDBDir      := flag.String("db-dir",      sentinel, "Directory for the mmdb files. Overrides DB_DIR. Default: /data")
	flagListenAddr := flag.String("listen-addr", sentinel, "Listen address. Overrides LISTEN_ADDR. Default: 127.0.0.1:8080")
	flagLicenseKey := flag.String("license-key", sentinel, "CDN license key (Basic auth token). Overrides LICENSE_KEY. Optional.")
	flag.Parse()

	resolve := func(flagVal, envKey, defaultVal string) string {
		if flagVal != sentinel {
			return flagVal
		}
		if v := os.Getenv(envKey); v != "" {
			return v
		}
		return defaultVal
	}

	dbURL      = resolve(*flagDBURL,      "DB_URL",      "")
	dbDir      = resolve(*flagDBDir,      "DB_DIR",      "/data")
	listenAddr = resolve(*flagListenAddr, "LISTEN_ADDR", "127.0.0.1:8080")
	licenseKey = resolve(*flagLicenseKey, "LICENSE_KEY", "")

	switch {
	case dbURL != "":
		log.Printf("Mode: peer sync from %s", dbURL)
		logProxyConfig(dbURL)
	case licenseKey != "":
		log.Printf("Mode: CDN CSV build — licensed")
		logProxyConfig(cdnPrefix2asURL)
	default:
		log.Printf("Mode: CDN CSV build — anonymous")
		logProxyConfig(cdnPrefix2asURL)
	}

	if err := os.MkdirAll(dbDir, 0755); err != nil {
		log.Fatalf("failed to create directory %s: %v", dbDir, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Load or build both databases.
	log.Println("Initialising prefix2as database...")
	if err := loadOrBuild(ctx, prefix2asDBFileName, prefix2asUpdateFile, &prefix2asDB, updatePrefix2as); err != nil {
		log.Fatalf("failed to initialise prefix2as database: %v", err)
	}

	log.Println("Initialising as2prefix database...")
	if err := loadOrBuild(ctx, as2prefixDBFileName, as2prefixUpdateFile, &as2prefixDB, updateAs2prefix); err != nil {
		log.Fatalf("failed to initialise as2prefix database: %v", err)
	}

	// Schedule periodic updates at 12:00 UTC (CDN publication slot).
	schedulePeriodicUpdate(ctx, "prefix2as", updatePrefix2as)
	schedulePeriodicUpdate(ctx, "as2prefix", updateAs2prefix)

	// HTTP routes.
	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/favicon.png", faviconHandler)
	http.HandleFunc("/openapi.json", openapiHandler)
	http.HandleFunc("/api/v1/asprefix", lookupHandler())
	http.HandleFunc("/db/prefix2as", getDBHandler(prefix2asDBFileName))
	http.HandleFunc("/db/as2prefix", getDBHandler(as2prefixDBFileName))

	srv := &http.Server{
		Addr:         listenAddr,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	log.Printf("http2prefix server listening on %s", listenAddr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}
