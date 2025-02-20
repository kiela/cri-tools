/*
Copyright 2022 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/docker/go-units"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
	"golang.org/x/net/context"
	cri "k8s.io/cri-api/pkg/apis"
	pb "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type podStatsOptions struct {
	// id of the pod sandbox
	id string

	// sample is the duration for sampling cpu usage.
	sample time.Duration

	// labels are selectors for the sandbox
	labels map[string]string

	// output format
	output string

	// live watch
	watch bool
}

var podStatsCommand = &cli.Command{
	Name:                   "statsp",
	Usage:                  "List pod resource usage statistics",
	UseShortOptionHandling: true,
	ArgsUsage:              "[ID]",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "id",
			Value: "",
			Usage: "Filter by pod id",
		},
		&cli.StringSliceFlag{
			Name:  "label",
			Usage: "Filter by key=value label",
		},
		&cli.StringFlag{
			Name:    "output",
			Aliases: []string{"o"},
			Usage:   "Output format, One of: json|yaml|table",
		},
		&cli.IntFlag{
			Name:    "seconds",
			Aliases: []string{"s"},
			Value:   1,
			Usage:   "Sample duration for CPU usage in seconds",
		},
		&cli.BoolFlag{
			Name:    "watch",
			Aliases: []string{"w"},
			Usage:   "Watch pod resources",
		},
	},
	Action: func(c *cli.Context) error {
		id := c.String("id")
		if id == "" && c.NArg() > 0 {
			id = c.Args().First()
		}
		if c.NArg() > 1 {
			return cli.ShowSubcommandHelp(c)
		}

		client, err := getRuntimeService(c, 0)
		if err != nil {
			return fmt.Errorf("get runtime service: %w", err)
		}

		opts := podStatsOptions{
			id:     id,
			sample: time.Duration(c.Int("seconds")) * time.Second,
			output: c.String("output"),
			watch:  c.Bool("watch"),
		}
		opts.labels, err = parseLabelStringSlice(c.StringSlice("label"))
		if err != nil {
			return fmt.Errorf("parse pod labels: %w", err)
		}

		if err := podStats(c.Context, client, opts); err != nil {
			return fmt.Errorf("get pod stats: %w", err)
		}

		return nil
	},
}

type podStatsByID []*pb.PodSandboxStats

func (c podStatsByID) Len() int      { return len(c) }
func (c podStatsByID) Swap(i, j int) { c[i], c[j] = c[j], c[i] }
func (c podStatsByID) Less(i, j int) bool {
	return c[i].Attributes.Id < c[j].Attributes.Id
}

func podStats(
	c context.Context,
	client cri.RuntimeService,
	opts podStatsOptions,
) error {
	filter := &pb.PodSandboxStatsFilter{}
	if opts.id != "" {
		filter.Id = opts.id
	}
	if opts.labels != nil {
		filter.LabelSelector = opts.labels
	}

	display := newTableDisplay(20, 1, 3, ' ', 0)
	if opts.watch {
		displayErrCh := make(chan error, 1)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		watchCtx, cancelFn := context.WithCancel(context.Background())
		defer cancelFn()

		// Put the displayPodStats in another goroutine, because it might be
		// time consuming with lots of pods and we want to cancel it
		// ASAP when user hit CtrlC
		go func() {
			for range ticker.C {
				if err := displayPodStats(watchCtx, client, filter, display, opts); err != nil {
					displayErrCh <- err
					break
				}
			}
		}()

		// listen for CtrlC or error
		select {
		case <-SetupInterruptSignalHandler():
			cancelFn()
			return nil
		case err := <-displayErrCh:
			return err
		}
	}

	if err := displayPodStats(c, client, filter, display, opts); err != nil {
		return fmt.Errorf("display pod stats: %w", err)
	}

	return nil
}

func displayPodStats(
	c context.Context,
	client cri.RuntimeService,
	filter *pb.PodSandboxStatsFilter,
	display *display,
	opts podStatsOptions,
) error {
	stats, err := getPodSandboxStats(client, filter)
	if err != nil {
		return err
	}

	response := &pb.ListPodSandboxStatsResponse{Stats: stats}
	switch opts.output {
	case "json":
		return outputProtobufObjAsJSON(response)
	case "yaml":
		return outputProtobufObjAsYAML(response)
	}

	oldStats := make(map[string]*pb.PodSandboxStats)
	for _, s := range stats {
		if c.Err() != nil {
			return c.Err()
		}
		oldStats[s.Attributes.Id] = s
	}

	time.Sleep(opts.sample)

	stats, err = getPodSandboxStats(client, filter)
	if err != nil {
		return err
	}

	display.AddRow([]string{columnPodName, columnPodID, columnCPU, columnMemory})
	for _, s := range stats {
		if c.Err() != nil {
			return c.Err()
		}
		id := getTruncatedID(s.Attributes.Id, "")

		var cpu, mem uint64
		var ts int64

		linux := s.GetLinux()
		windows := s.GetWindows()

		if linux != nil && windows != nil {
			return fmt.Errorf("pod %q has both linux and windows stats which is not supported", id)
		}

		if linux != nil {
			cpu = linux.GetCpu().GetUsageCoreNanoSeconds().GetValue()
			mem = linux.GetMemory().GetWorkingSetBytes().GetValue()
			ts = linux.GetCpu().GetTimestamp()
		} else if windows != nil {
			cpu = windows.GetCpu().GetUsageCoreNanoSeconds().GetValue()
			mem = windows.GetMemory().GetWorkingSetBytes().GetValue()
			ts = windows.GetCpu().GetTimestamp()
		}

		if cpu == 0 && mem == 0 {
			// Skip without data, maybe windows pod
			continue
		}

		var oldCpu uint64
		var oldCpuTs int64
		old, ok := oldStats[s.Attributes.Id]
		if !ok {
			// Skip new pod
			continue
		}

		oldLinux := old.GetLinux()
		oldWindows := old.GetWindows()
		if linux != nil {
			oldCpuTs = oldLinux.GetCpu().GetTimestamp()
			oldCpu = oldLinux.GetCpu().GetUsageCoreNanoSeconds().GetValue()
		} else if windows != nil {
			oldCpuTs = oldWindows.GetCpu().GetTimestamp()
			oldCpu = oldWindows.GetCpu().GetUsageCoreNanoSeconds().GetValue()
		}

		var cpuPerc float64
		if cpu != 0 {
			// Only generate cpuPerc for running sandbox
			duration := ts - oldCpuTs
			if duration == 0 {
				return errors.New("cpu stat is not updated during sample")
			}
			cpuPerc = float64(cpu-oldCpu) / float64(duration) * 100
		}
		display.AddRow([]string{
			s.Attributes.GetMetadata().GetName(),
			id,
			fmt.Sprintf("%.2f", cpuPerc),
			units.HumanSize(float64(mem)),
		})

	}
	display.ClearScreen()
	display.Flush()

	return nil
}

func getPodSandboxStats(
	client cri.RuntimeService,
	filter *pb.PodSandboxStatsFilter,
) ([]*pb.PodSandboxStats, error) {
	logrus.Debugf("PodSandboxStatsFilter: %v", filter)

	stats, err := client.ListPodSandboxStats(context.TODO(), filter)
	if err != nil {
		return nil, fmt.Errorf("list pod sandbox stats: %w", err)
	}
	logrus.Debugf("Stats: %v", stats)

	sort.Sort(podStatsByID(stats))

	return stats, nil
}
