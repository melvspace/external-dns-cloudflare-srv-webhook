package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	cloudflare "github.com/cloudflare/cloudflare-go"
)

const webhookContentType = "application/external.dns.webhook+json;version=1"

// ---------- external-dns types ----------

type ProviderSpecificProperty struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Endpoint struct {
	DNSName          string                     `json:"dnsName"`
	Targets          []string                   `json:"targets"`
	RecordType       string                     `json:"recordType"`
	RecordTTL        int64                      `json:"recordTTL,omitempty"`
	Labels           map[string]string          `json:"labels,omitempty"`
	ProviderSpecific []ProviderSpecificProperty `json:"providerSpecific,omitempty"`
}

func (ep *Endpoint) getProviderSpecific(name string) (string, bool) {
	for _, p := range ep.ProviderSpecific {
		if p.Name == name {
			return p.Value, true
		}
	}
	return "", false
}

type Changes struct {
	Create    []*Endpoint `json:"Create"`
	UpdateOld []*Endpoint `json:"UpdateOld"`
	UpdateNew []*Endpoint `json:"UpdateNew"`
	Delete    []*Endpoint `json:"Delete"`
}

// ---------- proxy state ----------

type proxy struct {
	cf             *cloudflare.API
	domainFilter   []string
	proxiedDefault bool
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

	api, err := cloudflare.NewWithAPIToken(token)
	if err != nil {
		return nil, fmt.Errorf("creating Cloudflare client: %w", err)
	}

	proxiedDefault := strings.EqualFold(os.Getenv("CF_PROXIED"), "true")

	p := &proxy{
		cf:             api,
		domainFilter:   filters,
		proxiedDefault: proxiedDefault,
		zones:          make(map[string]string),
	}

	log.Printf("proxied default: %v", proxiedDefault)

	if err := p.loadZones(); err != nil {
		return nil, fmt.Errorf("loading zones: %w", err)
	}

	return p, nil
}

// ---------- zone helpers ----------

func (p *proxy) loadZones() error {
	zones, err := p.cf.ListZones(context.Background())
	if err != nil {
		return err
	}

	for _, zone := range zones {
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

func (p *proxy) zoneIDForName(dnsName string) (string, error) {
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
		return "", fmt.Errorf("no zone found for %q", dnsName)
	}
	return bestID, nil
}

// ---------- record listing ----------

func (p *proxy) listAllRecords() ([]*Endpoint, error) {
	var endpoints []*Endpoint

	for _, zoneID := range p.zones {
		rc := cloudflare.ZoneIdentifier(zoneID)
		records, _, err := p.cf.ListDNSRecords(context.Background(), rc, cloudflare.ListDNSRecordsParams{})
		if err != nil {
			return nil, fmt.Errorf("listing records for zone %s: %w", zoneID, err)
		}

		for _, rec := range records {
			ep := cfRecordToEndpoint(rec)
			if ep != nil {
				endpoints = append(endpoints, ep)
			}
		}
	}

	return endpoints, nil
}

func cfRecordToEndpoint(rec cloudflare.DNSRecord) *Endpoint {
	switch rec.Type {
	case "A", "AAAA", "CNAME", "TXT":
		ep := &Endpoint{
			DNSName:    rec.Name,
			Targets:    []string{rec.Content},
			RecordType: rec.Type,
			RecordTTL:  int64(rec.TTL),
		}
		if rec.Proxied != nil && *rec.Proxied {
			ep.ProviderSpecific = []ProviderSpecificProperty{
				{Name: "external-dns.alpha.kubernetes.io/cloudflare-proxied", Value: "true"},
			}
		}
		return ep
	case "SRV":
		if rec.Data == nil {
			return nil
		}
		data, ok := rec.Data.(map[string]interface{})
		if !ok {
			return nil
		}
		priority := int(toFloat64(data["priority"]))
		weight := int(toFloat64(data["weight"]))
		port := int(toFloat64(data["port"]))
		target := strings.TrimSuffix(fmt.Sprintf("%v", data["target"]), ".")
		targetStr := fmt.Sprintf("%d %d %d %s", priority, weight, port, target)
		return &Endpoint{
			DNSName:    rec.Name,
			Targets:    []string{targetStr},
			RecordType: "SRV",
			RecordTTL:  int64(rec.TTL),
		}
	default:
		return nil
	}
}

func toFloat64(v interface{}) float64 {
	if v == nil {
		return 0
	}
	f, _ := v.(float64)
	return f
}

// ---------- record creation ----------

func (p *proxy) createRecord(ep *Endpoint) error {
	zoneID, err := p.zoneIDForName(ep.DNSName)
	if err != nil {
		return err
	}

	rc := cloudflare.ZoneIdentifier(zoneID)

	ttl := int(ep.RecordTTL)
	if ttl == 0 {
		ttl = 1 // automatic
	}

	for _, target := range ep.Targets {
		params := cloudflare.CreateDNSRecordParams{
			Type: ep.RecordType,
			Name: ep.DNSName,
			TTL:  ttl,
		}

		if ep.RecordType == "SRV" {
			data, err := parseSRVTarget(target)
			if err != nil {
				return fmt.Errorf("parsing SRV target %q: %w", target, err)
			}
			params.Data = data
		} else {
			params.Content = target

			if ep.RecordType == "A" || ep.RecordType == "AAAA" || ep.RecordType == "CNAME" {
				proxied := p.proxiedDefault
				if v, ok := ep.getProviderSpecific("external-dns.alpha.kubernetes.io/cloudflare-proxied"); ok {
					proxied = strings.EqualFold(v, "true")
				}
				params.Proxied = cloudflare.BoolPtr(proxied)
			}
		}

		if _, err := p.cf.CreateDNSRecord(context.Background(), rc, params); err != nil {
			return fmt.Errorf("Cloudflare create failed for %s %s: %w", ep.RecordType, ep.DNSName, err)
		}

		log.Printf("created %s %s → %s", ep.RecordType, ep.DNSName, target)
	}
	return nil
}

// parseSRVTarget parses "priority weight port target" into a map for the Cloudflare SDK.
func parseSRVTarget(s string) (map[string]interface{}, error) {
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
	return map[string]interface{}{
		"priority": priority,
		"weight":   weight,
		"port":     port,
		"target":   target,
	}, nil
}

// ---------- record deletion ----------

func (p *proxy) deleteRecord(ep *Endpoint) error {
	zoneID, err := p.zoneIDForName(ep.DNSName)
	if err != nil {
		return err
	}

	rc := cloudflare.ZoneIdentifier(zoneID)
	records, _, err := p.cf.ListDNSRecords(context.Background(), rc, cloudflare.ListDNSRecordsParams{
		Name: ep.DNSName,
		Type: ep.RecordType,
	})
	if err != nil {
		return fmt.Errorf("listing records for deletion %s %s: %w", ep.RecordType, ep.DNSName, err)
	}

	for _, rec := range records {
		if err := p.cf.DeleteDNSRecord(context.Background(), rc, rec.ID); err != nil {
			return fmt.Errorf("deleting record %s: %w", rec.ID, err)
		}
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
