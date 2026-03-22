package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
)

const (
	webhookContentType = "application/external.dns.webhook+json;version=1"
	cfBaseURL          = "https://api.cloudflare.com/client/v4"
)

// ---------- external-dns types ----------

type Endpoint struct {
	DNSName    string            `json:"dnsName"`
	Targets    []string          `json:"targets"`
	RecordType string            `json:"recordType"`
	RecordTTL  int64             `json:"recordTTL,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
}

type Changes struct {
	Create    []*Endpoint `json:"Create"`
	UpdateOld []*Endpoint `json:"UpdateOld"`
	UpdateNew []*Endpoint `json:"UpdateNew"`
	Delete    []*Endpoint `json:"Delete"`
}

// ---------- Cloudflare API types ----------

type cfZone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type cfZoneListResponse struct {
	Result []cfZone `json:"result"`
	Success bool     `json:"success"`
}

type cfDNSRecord struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Name    string          `json:"name"`
	Content string          `json:"content,omitempty"`
	TTL     int64           `json:"ttl,omitempty"`
	Proxied bool            `json:"proxied,omitempty"`
	Data    *cfSRVData      `json:"data,omitempty"`
}

type cfSRVData struct {
	Priority int    `json:"priority"`
	Weight   int    `json:"weight"`
	Port     int    `json:"port"`
	Target   string `json:"target"`
}

type cfDNSListResponse struct {
	Result  []cfDNSRecord `json:"result"`
	Success bool          `json:"success"`
}

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cfDNSCreateResponse struct {
	Result  cfDNSRecord `json:"result"`
	Success bool        `json:"success"`
	Errors  []cfError   `json:"errors"`
}

// ---------- proxy state ----------

type proxy struct {
	apiToken     string
	domainFilter []string
	// zone name -> zone ID
	zones map[string]string
}

func newProxy() (*proxy, error) {
	token := os.Getenv("CF_API_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("CF_API_TOKEN is required")
	}

	filterEnv := os.Getenv("CF_DOMAIN_FILTER")
	var filters []string
	for _, f := range strings.Split(filterEnv, ",") {
		f = strings.TrimSpace(f)
		if f != "" {
			filters = append(filters, f)
		}
	}

	p := &proxy{
		apiToken:     token,
		domainFilter: filters,
		zones:        make(map[string]string),
	}

	if err := p.loadZones(); err != nil {
		return nil, fmt.Errorf("loading zones: %w", err)
	}

	return p, nil
}

// ---------- Cloudflare helpers ----------

func (p *proxy) cfRequest(method, path string, body interface{}) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, cfBaseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.apiToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (p *proxy) loadZones() error {
	resp, err := p.cfRequest("GET", "/zones?per_page=100", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result cfZoneListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if !result.Success {
		return fmt.Errorf("Cloudflare zones API returned success=false")
	}

	for _, zone := range result.Result {
		if len(p.domainFilter) == 0 {
			p.zones[zone.Name] = zone.ID
			continue
		}
		for _, f := range p.domainFilter {
			if zone.Name == f || strings.HasSuffix(zone.Name, "."+f) || strings.HasSuffix(f, "."+zone.Name) {
				p.zones[zone.Name] = zone.ID
				break
			}
		}
	}

	log.Printf("loaded %d zone(s): %v", len(p.zones), p.zoneNames())
	return nil
}

func (p *proxy) zoneNames() []string {
	names := make([]string, 0, len(p.zones))
	for name := range p.zones {
		names = append(names, name)
	}
	return names
}

// zoneIDForName returns the zone ID that best matches the given DNS name.
func (p *proxy) zoneIDForName(dnsName string) (string, string, error) {
	best := ""
	bestID := ""
	for zoneName, zoneID := range p.zones {
		if dnsName == zoneName || strings.HasSuffix(dnsName, "."+zoneName) {
			if len(zoneName) > len(best) {
				best = zoneName
				bestID = zoneID
			}
		}
	}
	if bestID == "" {
		return "", "", fmt.Errorf("no zone found for %q", dnsName)
	}
	return bestID, best, nil
}

// ---------- record listing ----------

func (p *proxy) listAllRecords() ([]*Endpoint, error) {
	var endpoints []*Endpoint

	for zoneName, zoneID := range p.zones {
		_ = zoneName
		path := fmt.Sprintf("/zones/%s/dns_records?per_page=100", zoneID)
		resp, err := p.cfRequest("GET", path, nil)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		var result cfDNSListResponse
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, err
		}
		if !result.Success {
			return nil, fmt.Errorf("Cloudflare dns_records API returned success=false for zone %s", zoneID)
		}

		for _, rec := range result.Result {
			ep := cfRecordToEndpoint(rec)
			if ep != nil {
				endpoints = append(endpoints, ep)
			}
		}
	}

	return endpoints, nil
}

func cfRecordToEndpoint(rec cfDNSRecord) *Endpoint {
	switch rec.Type {
	case "A", "AAAA", "CNAME", "TXT":
		return &Endpoint{
			DNSName:    rec.Name,
			Targets:    []string{rec.Content},
			RecordType: rec.Type,
			RecordTTL:  rec.TTL,
		}
	case "SRV":
		if rec.Data == nil {
			return nil
		}
		target := strings.TrimSuffix(rec.Data.Target, ".")
		targetStr := fmt.Sprintf("%d %d %d %s",
			rec.Data.Priority, rec.Data.Weight, rec.Data.Port, target)
		return &Endpoint{
			DNSName:    rec.Name,
			Targets:    []string{targetStr},
			RecordType: "SRV",
			RecordTTL:  rec.TTL,
		}
	default:
		return nil
	}
}

// ---------- record creation ----------

func (p *proxy) createRecord(ep *Endpoint) error {
	zoneID, _, err := p.zoneIDForName(ep.DNSName)
	if err != nil {
		return err
	}

	for _, target := range ep.Targets {
		var rec cfDNSRecord
		rec.Type = ep.RecordType
		rec.Name = ep.DNSName
		rec.TTL = ep.RecordTTL
		if rec.TTL == 0 {
			rec.TTL = 1 // automatic
		}

		if ep.RecordType == "SRV" {
			data, err := parseSRVTarget(target)
			if err != nil {
				return fmt.Errorf("parsing SRV target %q: %w", target, err)
			}
			rec.Data = data
		} else {
			rec.Content = target
		}

		path := fmt.Sprintf("/zones/%s/dns_records", zoneID)
		resp, err := p.cfRequest("POST", path, rec)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		var result cfDNSCreateResponse
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return err
		}
		if !result.Success {
			return fmt.Errorf("Cloudflare create failed for %s %s: %v", ep.RecordType, ep.DNSName, result.Errors)
		}

		log.Printf("created %s %s → %s", ep.RecordType, ep.DNSName, target)
	}
	return nil
}

// parseSRVTarget parses "priority weight port target" into cfSRVData.
func parseSRVTarget(s string) (*cfSRVData, error) {
	parts := strings.Fields(s)
	if len(parts) != 4 {
		return nil, fmt.Errorf("expected 4 fields, got %d in %q", len(parts), s)
	}
	priority, err := strconv.Atoi(parts[0])
	if err != nil {
		return nil, fmt.Errorf("priority: %w", err)
	}
	weight, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("weight: %w", err)
	}
	port, err := strconv.Atoi(parts[2])
	if err != nil {
		return nil, fmt.Errorf("port: %w", err)
	}
	target := parts[3]
	if !strings.HasSuffix(target, ".") {
		target += "."
	}
	return &cfSRVData{
		Priority: priority,
		Weight:   weight,
		Port:     port,
		Target:   target,
	}, nil
}

// ---------- record deletion ----------

func (p *proxy) deleteRecord(ep *Endpoint) error {
	zoneID, _, err := p.zoneIDForName(ep.DNSName)
	if err != nil {
		return err
	}

	path := fmt.Sprintf("/zones/%s/dns_records?name=%s&type=%s",
		zoneID, ep.DNSName, ep.RecordType)
	resp, err := p.cfRequest("GET", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result cfDNSListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if !result.Success {
		return fmt.Errorf("listing records for deletion failed: %s %s", ep.RecordType, ep.DNSName)
	}

	for _, rec := range result.Result {
		delPath := fmt.Sprintf("/zones/%s/dns_records/%s", zoneID, rec.ID)
		delResp, err := p.cfRequest("DELETE", delPath, nil)
		if err != nil {
			return err
		}
		delResp.Body.Close()
		log.Printf("deleted %s %s (id=%s)", ep.RecordType, ep.DNSName, rec.ID)
	}

	return nil
}

// ---------- HTTP handlers ----------

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", webhookContentType)
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v interface{}) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

func (p *proxy) handleNegotiate(w http.ResponseWriter, r *http.Request) {
	filters := p.domainFilter
	if filters == nil {
		filters = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"domainFilter": map[string]interface{}{
			"filters": filters,
		},
	})
}

func (p *proxy) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "OK")
}

func (p *proxy) handleGetRecords(w http.ResponseWriter, r *http.Request) {
	endpoints, err := p.listAllRecords()
	if err != nil {
		log.Printf("ERROR listing records: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if endpoints == nil {
		endpoints = []*Endpoint{}
	}
	writeJSON(w, http.StatusOK, endpoints)
}

func (p *proxy) handleApplyChanges(w http.ResponseWriter, r *http.Request) {
	var changes Changes
	if err := readJSON(r, &changes); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Delete old records first (UpdateOld + Delete)
	for _, ep := range changes.Delete {
		if err := p.deleteRecord(ep); err != nil {
			log.Printf("ERROR deleting %s %s: %v", ep.RecordType, ep.DNSName, err)
		}
	}
	for _, ep := range changes.UpdateOld {
		if err := p.deleteRecord(ep); err != nil {
			log.Printf("ERROR deleting (update-old) %s %s: %v", ep.RecordType, ep.DNSName, err)
		}
	}

	// Create new records (Create + UpdateNew)
	for _, ep := range changes.Create {
		if err := p.createRecord(ep); err != nil {
			log.Printf("ERROR creating %s %s: %v", ep.RecordType, ep.DNSName, err)
		}
	}
	for _, ep := range changes.UpdateNew {
		if err := p.createRecord(ep); err != nil {
			log.Printf("ERROR creating (update-new) %s %s: %v", ep.RecordType, ep.DNSName, err)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

func (p *proxy) handleAdjustEndpoints(w http.ResponseWriter, r *http.Request) {
	var endpoints []*Endpoint
	if err := readJSON(r, &endpoints); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Pass through unchanged
	writeJSON(w, http.StatusOK, endpoints)
}

func (p *proxy) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", p.handleNegotiate)
	mux.HandleFunc("GET /healthz", p.handleHealthz)
	mux.HandleFunc("GET /records", p.handleGetRecords)
	mux.HandleFunc("POST /records", p.handleApplyChanges)
	mux.HandleFunc("POST /adjustendpoints", p.handleAdjustEndpoints)
	return mux
}

// ---------- main ----------

func main() {
	p, err := newProxy()
	if err != nil {
		log.Fatalf("init: %v", err)
	}

	addr := ":8888"
	log.Printf("starting external-dns Cloudflare webhook proxy on %s", addr)
	if err := http.ListenAndServe(addr, p.routes()); err != nil {
		log.Fatalf("server: %v", err)
	}
}
