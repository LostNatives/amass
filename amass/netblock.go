// Copyright 2017 Jeff Foley. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package amass

import (
	"net"
	"time"

	"github.com/caffix/recon"
	//"github.com/yl2chen/cidranger"
)

type cidrData struct {
	Netblock *net.IPNet
	ASN      int
}

type asnData struct {
	Record    *recon.ASRecord
	Netblocks []string
}

type NetblockService struct {
	BaseAmassService

	// Queue for requests waiting for Shadowserver data
	queue []*AmassRequest

	// CIDR data cached from the Shadowserver requests
	cidrCache map[string]*cidrData

	// Fast lookup of an IP across all known CIDRs
	//networks cidranger.Ranger

	// ASN data cached from the Shadowserver requests
	asnCache map[int]*asnData
}

func NewNetblockService(in, out chan *AmassRequest, config *AmassConfig) *NetblockService {
	ns := &NetblockService{
		cidrCache: make(map[string]*cidrData),
		//networks:  cidranger.NewPCTrieRanger(),
		asnCache: make(map[int]*asnData),
	}

	ns.BaseAmassService = *NewBaseAmassService("Netblock Service", config, ns)

	ns.input = in
	ns.output = out
	return ns
}

func (ns *NetblockService) OnStart() error {
	ns.BaseAmassService.OnStart()

	go ns.processRequests()
	go ns.initialRequests()
	return nil
}

func (ns *NetblockService) OnStop() error {
	ns.BaseAmassService.OnStop()
	return nil
}

func (ns *NetblockService) initialRequests() {
	// Do root domain names need to be discovered?
	if !ns.Config().AddDomains {
		return
	}
	// Enter all ASN requests into the queue
	for _, asn := range ns.Config().ASNs {
		ns.add(&AmassRequest{
			ASN:        asn,
			noSweep:    true,
			addDomains: true,
		})
	}
	// Enter all CIDR requests into the queue
	for _, cidr := range ns.Config().CIDRs {
		ns.add(&AmassRequest{
			Netblock:   cidr,
			noSweep:    true,
			addDomains: true,
		})
	}
	// Enter all IP address requests from ranges
	for _, rng := range ns.Config().Ranges {
		ips := RangeHosts(rng)

		for _, ip := range ips {
			ns.add(&AmassRequest{
				Address:    ip,
				noSweep:    true,
				addDomains: true,
			})
		}
	}
	// Enter all IP address requests into the queue
	for _, ip := range ns.Config().IPs {
		ns.add(&AmassRequest{
			Address:    ip.String(),
			noSweep:    true,
			addDomains: true,
		})
	}
}

func (ns *NetblockService) processRequests() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()

	pull := time.NewTicker(3 * time.Second)
	defer pull.Stop()
loop:
	for {
		select {
		case req := <-ns.Input():
			ns.SetActive(true)
			if !ns.completeAddrRequest(req) {
				ns.add(req)
			}
		case <-pull.C:
			go ns.performLookup()
		case <-t.C:
			ns.SetActive(false)
		case <-ns.Quit():
			break loop
		}
	}
}

func (ns *NetblockService) add(req *AmassRequest) {
	ns.Lock()
	defer ns.Unlock()

	ns.queue = append(ns.queue, req)
}

func (ns *NetblockService) next() *AmassRequest {
	ns.Lock()
	defer ns.Unlock()

	var next *AmassRequest
	if len(ns.queue) == 1 {
		next = ns.queue[0]
		ns.queue = []*AmassRequest{}
	} else if len(ns.queue) > 1 {
		next = ns.queue[0]
		ns.queue = ns.queue[1:]
	}
	return next
}

func (ns *NetblockService) performLookup() {
	req := ns.next()
	// Empty as much of the queue as possible
	for req != nil {
		// Can we send it out now?
		if !ns.completeAddrRequest(req) {
			break
		}
		req = ns.next()
	}
	// Empty queue?
	if req == nil {
		return
	}
	ns.SetActive(true)
	// Which type of lookup will be performed?
	if req.Address != "" {
		ns.IPLookup(req)
	} else if req.Netblock != nil {
		ns.CIDRLookup(req)
	} else if req.ASN != 0 {
		ns.ASNLookup(req)
	}
}

func (ns *NetblockService) sendRequest(req *AmassRequest, cidr *cidrData, asn *asnData) {
	var required, pass bool

	ns.SetActive(true)
	// Add the netblock, etc to the request
	req.Netblock = cidr.Netblock
	req.ASN = asn.Record.ASN
	req.ISP = asn.Record.ISP
	// Check if this request should be stopped due to infrastructure contraints
	if len(ns.Config().ASNs) > 0 {
		required = true
		for _, asn := range ns.Config().ASNs {
			if asn == req.ASN {
				pass = true
				break
			}
		}
	}
	if !pass && len(ns.Config().CIDRs) > 0 {
		required = true
		for _, cidr := range ns.Config().CIDRs {
			if cidr.String() == req.Netblock.String() {
				pass = true
				break
			}
		}
	}
	if !pass && len(ns.Config().Ranges) > 0 {
		required = true
		for _, rng := range ns.Config().Ranges {
			ips := RangeHosts(rng)
			for _, ip := range ips {
				if ip == req.Address {
					pass = true
					break
				}
			}
			if pass {
				break
			}
		}
	}
	if !pass && len(ns.Config().IPs) > 0 {
		required = true
		for _, ip := range ns.Config().IPs {
			if ip.String() == req.Address {
				pass = true
				break
			}
		}
	}
	if required && !pass {
		return
	}
	// Send it on it's way
	ns.SendOut(req)
}

func (ns *NetblockService) cidrCacheFetch(cidr string) *cidrData {
	ns.Lock()
	defer ns.Unlock()

	var result *cidrData
	if data, found := ns.cidrCache[cidr]; found {
		result = data
	}
	return result
}

func (ns *NetblockService) cidrCacheInsert(cidr string, entry *cidrData) {
	ns.Lock()
	defer ns.Unlock()

	ns.cidrCache[cidr] = entry
	//ns.networks.Insert(cidranger.NewBasicRangerEntry(*entry.Netblock))
}

func (ns *NetblockService) insertAllNetblocks(netblocks []string, asn int) {
	for _, nb := range netblocks {
		_, cidr, err := net.ParseCIDR(nb)
		if err != nil {
			continue
		}

		ns.cidrCacheInsert(cidr.String(), &cidrData{
			Netblock: cidr,
			ASN:      asn,
		})
	}
}

func (ns *NetblockService) asnCacheFetch(asn int) *asnData {
	ns.Lock()
	defer ns.Unlock()

	var result *asnData
	if data, found := ns.asnCache[asn]; found {
		result = data
	}
	return result
}

func (ns *NetblockService) asnCacheInsert(asn int, entry *asnData) {
	ns.Lock()
	defer ns.Unlock()

	ns.asnCache[asn] = entry
}

func (ns *NetblockService) ASNLookup(req *AmassRequest) {
	data := ns.asnCacheFetch(req.ASN)
	// Does the data need to be obtained?
	if data == nil {
		// Get the netblocks associated with the ASN
		netblocks, err := recon.ASNToNetblocks(req.ASN)
		if err != nil {
			return
		}
		// Insert all the new netblocks into the cache
		ns.insertAllNetblocks(netblocks, req.ASN)
		// Get the AS recond as well
		_, cidr, err := net.ParseCIDR(netblocks[0])
		if err != nil {
			return
		}
		ips := NetHosts(cidr)
		record, err := recon.IPToASRecord(ips[0])
		if err != nil {
			return
		}

		data = &asnData{
			Record:    record,
			Netblocks: netblocks,
		}
		// Insert the AS record into the cache
		ns.asnCacheInsert(record.ASN, data)
	}
	// For every netblock, initiate subdomain name discovery
	for _, cidr := range data.Netblocks {
		c := ns.cidrCacheFetch(cidr)
		if c == nil {
			continue
		}
		// Send the request for this netblock
		ns.sendRequest(&AmassRequest{addDomains: req.addDomains}, c, data)
	}
}

func (ns *NetblockService) CIDRLookup(req *AmassRequest) {
	data := ns.cidrCacheFetch(req.Netblock.String())
	// Does the data need to be obtained?
	if data == nil {
		// Get the AS recond as well
		ips := NetHosts(req.Netblock)
		record, netblocks := ns.ipToData(ips[0])
		if record == nil {
			return
		}
		// Insert all the new netblocks into the cache
		ns.insertAllNetblocks(netblocks, record.ASN)

		data = &cidrData{
			Netblock: req.Netblock,
			ASN:      record.ASN,
		}
		// Insert the AS record into the cache
		ns.asnCacheInsert(record.ASN, &asnData{
			Record:    record,
			Netblocks: netblocks,
		})
	}
	// Grab the ASN data and send the request along
	a := ns.asnCacheFetch(data.ASN)
	ns.sendRequest(req, data, a)
}

func (ns *NetblockService) IPLookup(req *AmassRequest) {
	// Perform a Shadowserver lookup
	record, netblocks := ns.ipToData(req.Address)
	if record == nil {
		return
	}
	// Insert the new ASN data into the cache
	ns.asnCacheInsert(record.ASN, &asnData{
		Record:    record,
		Netblocks: netblocks,
	})
	// Insert all the new netblocks into the cache
	ns.insertAllNetblocks(netblocks, record.ASN)
	// Complete the request that started this lookup
	ns.completeAddrRequest(req)
}

func (ns *NetblockService) ipToData(addr string) (*recon.ASRecord, []string) {
	// Get the AS record for the IP address
	record, err := recon.IPToASRecord(addr)
	if err != nil {
		return nil, []string{}
	}
	// Get the netblocks associated with the ASN
	netblocks, err := recon.ASNToNetblocks(record.ASN)
	if err != nil {
		return nil, []string{}
	}
	return record, netblocks
}

func (ns *NetblockService) ipToCIDR(addr string) string {
	var result string

	ns.Lock()
	defer ns.Unlock()

	// Check the tree for which CIDR this IP address falls within
	/*entries, err := ns.networks.ContainingNetworks(net.ParseIP(addr))
	if err == nil {
		net := entries[0].Network()
		return net.String()
	}*/

	ip := net.ParseIP(addr)
	for cidr, data := range ns.cidrCache {
		if data.Netblock.Contains(ip) {
			result = cidr
			break
		}
	}
	return result
}

func (ns *NetblockService) completeAddrRequest(req *AmassRequest) bool {
	if req.Address == "" {
		return false
	}

	cidr := ns.ipToCIDR(req.Address)
	if cidr == "" {
		return false
	}

	c := ns.cidrCacheFetch(cidr)
	a := ns.asnCacheFetch(c.ASN)
	ns.sendRequest(req, c, a)
	return true
}
