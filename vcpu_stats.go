package main

// Per-vCPU scheduler statistics for nguyenhuukhoi/prometheus-libvirt-exporter.
//
// Drop this file next to prometheus-libvirt-exporter.go (same package main) and
// make the two small edits described in README.md.
//
// Why this is not derivable from what the exporter already collects:
// DomainGetInfo returns cpuTime, which says how much CPU the domain *used*. It
// cannot say how much the domain *wanted*. A VM at 20% CPU may be idle, or may
// be throttled by an oversubscribed host — those look identical in cpuTime.
// VIR_DOMAIN_STATS_VCPU closes that gap:
//
//	vcpu.<n>.delay  time the vCPU thread sat in the host run queue while
//	                runnable — exactly the steal time the guest sees
//	vcpu.<n>.wait   time the vCPU wanted to run while the host scheduler ran
//	                something else
//	vcpu.<n>.time   time the vCPU actually ran
//
// Host requirements (see README.md for the checks):
//   - libvirt >= 7.2 on the hypervisor for .delay
//   - kernel.sched_schedstats = 1 — BOTH .delay and .wait read kernel scheduler
//     accounting that is switched off by default. Without it the fields are
//     present but frozen at 0, which reads as "nothing is starved" rather than
//     "we cannot tell". That silent-zero case is the dangerous one.
//
// When the stats are unavailable this file reports nothing and never fails a
// scrape.

import (
	"strconv"
	"strings"

	"github.com/digitalocean/go-libvirt"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

const (
	// VIR_DOMAIN_STATS_VCPU from libvirt's virDomainStatsTypes enum. go-libvirt
	// is generated from the RPC protocol description and does not carry the C
	// header's enums, so the bit is spelled out here.
	domainStatsVCPU uint32 = 1 << 3

	// VIR_TYPED_PARAM_ULLONG — the discriminator go-libvirt puts in
	// TypedParamValue.D for unsigned long long values.
	typedParamULLong uint32 = 4

	// libvirt reports vcpu.<n>.time, .wait and .delay in nanoseconds. The
	// exporter's existing cpu_time metric already divides by the same constant.
	nanosecondsPerSecond = 1e9
)

// Same label set as every other domain metric, plus the vCPU index appended
// last — mirroring how libvirtDomainState appends stateDesc.
var vcpuStatLabels = []string{
	"domain", "instanceName", "instanceId", "flavorName",
	"userName", "userId", "projectName", "projectId",
	"host", "vcpu",
}

var (
	libvirtDomainVcpuTimeDesc = prometheus.NewDesc(
		prometheus.BuildFQName("libvirt", "domain_vcpu", "time_seconds_total"),
		"Time this virtual CPU spent running, in seconds.",
		vcpuStatLabels,
		nil)

	libvirtDomainVcpuWaitDesc = prometheus.NewDesc(
		prometheus.BuildFQName("libvirt", "domain_vcpu", "wait_seconds_total"),
		"Time this virtual CPU wanted to run while the host scheduler ran something else, in seconds.",
		vcpuStatLabels,
		nil)

	libvirtDomainVcpuDelayDesc = prometheus.NewDesc(
		prometheus.BuildFQName("libvirt", "domain_vcpu", "delay_seconds_total"),
		"Time this virtual CPU thread was enqueued by the host scheduler but waited in the run queue instead of running, in seconds. This is what the guest sees as steal time.",
		vcpuStatLabels,
		nil)
)

// parseVcpuStatField splits "vcpu.3.delay" into ("3", "delay").
//
// The reply also carries aggregate fields with no index (vcpu.current,
// vcpu.maximum); those are rejected here so the caller can ignore them.
func parseVcpuStatField(field string) (index string, suffix string, ok bool) {
	if !strings.HasPrefix(field, "vcpu.") {
		return "", "", false
	}

	rest := strings.TrimPrefix(field, "vcpu.")
	dot := strings.IndexByte(rest, '.')
	if dot <= 0 {
		return "", "", false
	}

	index, suffix = rest[:dot], rest[dot+1:]
	if _, err := strconv.Atoi(index); err != nil {
		return "", "", false
	}

	return index, suffix, true
}

// vcpuStatValue extracts an unsigned long long typed parameter.
//
// The XDR decoder hands the value back as interface{}; accept both signednesses
// rather than silently dropping samples if decoding differs by version.
func vcpuStatValue(value libvirt.TypedParamValue) (uint64, bool) {
	if value.D != typedParamULLong {
		return 0, false
	}

	switch n := value.I.(type) {
	case uint64:
		return n, true
	case int64:
		if n < 0 {
			return 0, false
		}
		return uint64(n), true
	}

	return 0, false
}

// CollectDomainVcpuStats reports per-vCPU scheduler statistics for one domain.
//
// Signature matches collectFunc so it can join the slice in CollectDomain,
// which means it only runs for domains that are already known to be active.
func CollectDomainVcpuStats(ch chan<- prometheus.Metric, l *libvirt.Libvirt, domain domainMeta, promLabels []string) (err error) {
	var records []libvirt.DomainStatsRecord
	if records, err = l.ConnectGetAllDomainStats([]libvirt.Domain{domain.libvirtDomain}, domainStatsVCPU, 0); err != nil {
		logger.Warn("failed to get vcpu stats",
			zap.String("domain", domain.domainName), zap.Error(err))
		// Returning nil on purpose: an older libvirt rejects this call on every
		// scrape, and an optional metric must not be reported as a collection
		// failure for the whole domain.
		return nil
	}

	for _, record := range records {
		for _, param := range record.Params {
			index, suffix, ok := parseVcpuStatField(param.Field)
			if !ok {
				continue
			}

			var desc *prometheus.Desc
			switch suffix {
			case "time":
				desc = libvirtDomainVcpuTimeDesc
			case "wait":
				desc = libvirtDomainVcpuWaitDesc
			case "delay":
				desc = libvirtDomainVcpuDelayDesc
			default:
				// .state and .halted are not counters; skip them here.
				continue
			}

			raw, ok := vcpuStatValue(param.Value)
			if !ok {
				continue
			}

			// Build a fresh slice rather than append() onto promLabels: the
			// caller reuses that slice for every metric of this domain, and
			// appending in a loop could write through a shared backing array.
			labels := make([]string, 0, len(promLabels)+1)
			labels = append(labels, promLabels...)
			labels = append(labels, index)

			ch <- prometheus.MustNewConstMetric(
				desc,
				prometheus.CounterValue,
				float64(raw)/nanosecondsPerSecond,
				labels...)
		}
	}

	return nil
}
