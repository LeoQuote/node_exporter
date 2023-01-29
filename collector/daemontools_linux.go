// Copyright 2015 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !nodaemontools
// +build !nodaemontools

package collector

import (
	"context"
	"fmt"
	"github.com/jehiah/go-daemontools"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	kingpin "gopkg.in/alecthomas/kingpin.v2"
)

var (
	unitIncludeSet bool
	unitInclude    = kingpin.Flag("collector.daemontools.svc-include", "Regexp of systemd units to include. Units must both match include and not match exclude to be included.").Default(".+").PreAction(func(c *kingpin.ParseContext) error {
		unitIncludeSet = true
		return nil
	}).String()
	unitExcludeSet bool
	unitExclude    = kingpin.Flag("collector.daemontools.svc-exclude", "Regexp of systemd units to exclude. Units must both match include and not match exclude to be included.").Default(".+\\.(automount|device|mount|scope|slice)").PreAction(func(c *kingpin.ParseContext) error {
		unitExcludeSet = true
		return nil
	}).String()
)

type daemontoolsCollector struct {
	svcDesc           *prometheus.Desc
	svcStartTimeDesc  *prometheus.Desc
	svcIncludePattern *regexp.Regexp
	svcExcludePattern *regexp.Regexp
	logger            log.Logger
}

var unitStatesName = []string{"up", "down"}

func init() {
	registerCollector("daemontools", defaultDisabled, NewSystemdCollector)
}

// NewSystemdCollector returns a new Collector exposing systemd statistics.
func NewSystemdCollector(logger log.Logger) (Collector, error) {
	const subsystem = "daemontools"

	svcDesc := prometheus.NewDesc(
		prometheus.BuildFQName(namespace, subsystem, "svc_status"),
		"Systemd unit", []string{"name", "state", "type"}, nil,
	)

	level.Info(logger).Log("msg", "Parsed flag --collector.systemd.unit-include", "flag", *unitInclude)
	unitIncludePattern := regexp.MustCompile(fmt.Sprintf("^(?:%s)$", *unitInclude))
	level.Info(logger).Log("msg", "Parsed flag --collector.systemd.unit-exclude", "flag", *unitExclude)
	unitExcludePattern := regexp.MustCompile(fmt.Sprintf("^(?:%s)$", *unitExclude))

	return &daemontoolsCollector{
		svcDesc:            svcDesc,
		svcStartTimeDesc:   svcStartTimeDesc,
		unitIncludePattern: unitIncludePattern,
		unitExcludePattern: unitExcludePattern,
		logger:             logger,
	}, nil
}

// Update gathers metrics from systemd.  Dbus collection is done in parallel
// to reduce wait time for responses.
func (c *daemontoolsCollector) Update(ch chan<- prometheus.Metric) error {
	begin := time.Now()
	conn, err := newSystemdDbusConn()
	if err != nil {
		return fmt.Errorf("couldn't get dbus connection: %w", err)
	}
	defer conn.Close()

	systemdVersion, systemdVersionFull := c.getSystemdVersion(conn)
	if systemdVersion < minSystemdVersionSystemState {
		level.Debug(c.logger).Log("msg", "Detected systemd version is lower than minimum, some systemd state and timer metrics will not be available", "current", systemdVersion, "minimum", minSystemdVersionSystemState)
	}
	ch <- prometheus.MustNewConstMetric(
		c.systemdVersionDesc,
		prometheus.GaugeValue,
		systemdVersion,
		systemdVersionFull,
	)

	allUnits, err := c.getAllUnits(conn)
	if err != nil {
		return fmt.Errorf("couldn't get units: %w", err)
	}
	level.Debug(c.logger).Log("msg", "getAllUnits took", "duration_seconds", time.Since(begin).Seconds())

	begin = time.Now()
	summary := summarizeUnits(allUnits)
	c.collectSummaryMetrics(ch, summary)
	level.Debug(c.logger).Log("msg", "collectSummaryMetrics took", "duration_seconds", time.Since(begin).Seconds())

	begin = time.Now()
	units := filterUnits(allUnits, c.unitIncludePattern, c.unitExcludePattern, c.logger)
	level.Debug(c.logger).Log("msg", "filterUnits took", "duration_seconds", time.Since(begin).Seconds())

	var wg sync.WaitGroup
	defer wg.Wait()

	wg.Add(1)
	go func() {
		defer wg.Done()
		begin = time.Now()
		c.collectSvcStatusMetrics(conn, ch, units)
		level.Debug(c.logger).Log("msg", "collectSvcStatusMetrics took", "duration_seconds", time.Since(begin).Seconds())
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		begin = time.Now()
		c.collectSvcStartTimeMetrics(conn, ch, units)
		level.Debug(c.logger).Log("msg", "collectSvcStartTimeMetrics took", "duration_seconds", time.Since(begin).Seconds())
	}()
	return err
}

func (c *daemontoolsCollector) collectSvcStatusMetrics(conn *dbus.Conn, ch chan<- prometheus.Metric, services []svc) {
	for _, svc := range services {
		serviceType := ""
		for _, stateName := range unitStatesName {
			isActive := 0.0
			if stateName == unit.ActiveState {
				isActive = 1.0
			}
			ch <- prometheus.MustNewConstMetric(
				c.unitDesc, prometheus.GaugeValue, isActive,
				svc, stateName, serviceType)
		}
		if *enableRestartsMetrics && strings.HasSuffix(unit.Name, ".service") {
			// NRestarts wasn't added until systemd 235.
			restartsCount, err := conn.GetUnitTypePropertyContext(context.TODO(), unit.Name, "Service", "NRestarts")
			if err != nil {
				level.Debug(c.logger).Log("msg", "couldn't get unit NRestarts", "unit", unit.Name, "err", err)
			} else {
				ch <- prometheus.MustNewConstMetric(
					c.nRestartsDesc, prometheus.CounterValue,
					float64(restartsCount.Value.Value().(uint32)), unit.Name)
			}
		}
	}
}

func (c *daemontoolsCollector) collectSvcStartTimeMetrics(conn *dbus.Conn, ch chan<- prometheus.Metric, units []unit) {
	var startTimeUsec uint64

	for _, unit := range units {
		if unit.ActiveState != "active" {
			startTimeUsec = 0
		} else {
			timestampValue, err := conn.GetUnitPropertyContext(context.TODO(), unit.Name, "ActiveEnterTimestamp")
			if err != nil {
				level.Debug(c.logger).Log("msg", "couldn't get unit StartTimeUsec", "unit", unit.Name, "err", err)
				continue
			}
			startTimeUsec = timestampValue.Value.Value().(uint64)
		}

		ch <- prometheus.MustNewConstMetric(
			c.unitStartTimeDesc, prometheus.GaugeValue,
			float64(startTimeUsec)/1e6, unit.Name)
	}
}

type svc struct {
	daemontools.Status
}

func (c *daemontoolsCollector) getAllUnits(conn *dbus.Conn) ([]unit, error) {
	allUnits, err := conn.ListUnitsContext(context.TODO())
	if err != nil {
		return nil, err
	}

	result := make([]unit, 0, len(allUnits))
	for _, status := range allUnits {
		unit := unit{
			UnitStatus: status,
		}
		result = append(result, unit)
	}

	return result, nil
}

func filterUnits(units []unit, includePattern, excludePattern *regexp.Regexp, logger log.Logger) []unit {
	filtered := make([]unit, 0, len(units))
	for _, unit := range units {
		if includePattern.MatchString(unit.Name) && !excludePattern.MatchString(unit.Name) && unit.LoadState == "loaded" {
			level.Debug(logger).Log("msg", "Adding unit", "unit", unit.Name)
			filtered = append(filtered, unit)
		} else {
			level.Debug(logger).Log("msg", "Ignoring unit", "unit", unit.Name)
		}
	}

	return filtered
}
