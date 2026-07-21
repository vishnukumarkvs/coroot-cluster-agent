package metrics

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/coroot/coroot-cluster-agent/common"
	"github.com/coroot/coroot-cluster-agent/flags"
	remoteapi "github.com/prometheus/client_golang/exp/api/remote"
	"github.com/prometheus/client_golang/prometheus"
	promCommon "github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/discovery"
	"github.com/prometheus/prometheus/discovery/kubernetes"
	"github.com/prometheus/prometheus/model/relabel"
	"github.com/prometheus/prometheus/scrape"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/remote"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/agent"
	"k8s.io/klog"
)

const (
	RemoteWriteTimeout  = 30 * time.Second
	RemoteFlushDeadline = time.Minute
	jobName             = "coroot-cluster-agent"
)

func (ms *Metrics) runScraper() error {
	logger := NewLogger()
	cfg := config.DefaultConfig
	cfg.GlobalConfig.ScrapeInterval = model.Duration(ms.scrapeInterval)
	cfg.GlobalConfig.ScrapeTimeout = model.Duration(ms.scrapeTimeout)
	cfg.RemoteWriteConfigs = append(cfg.RemoteWriteConfigs,
		&config.RemoteWriteConfig{
			URL:             &promCommon.URL{URL: ms.endpoint},
			Headers:         common.AuthHeaders(ms.apiKey),
			RemoteTimeout:   model.Duration(RemoteWriteTimeout),
			ProtobufMessage: remoteapi.WriteV1MessageType,
			QueueConfig:     config.DefaultQueueConfig,
			HTTPClientConfig: promCommon.HTTPClientConfig{
				TLSConfig: promCommon.TLSConfig{
					InsecureSkipVerify: *flags.InsecureSkipVerify,
					CAFile:             *flags.CAFile,
				},
			},
		},
	)
	targets := []model.LabelSet{{
		model.AddressLabel:  model.LabelValue(ms.listenAddr),
		model.InstanceLabel: model.LabelValue(ms.listenAddr),
	}}
	if ms.ksmAddr != "" {
		targets = append(targets, model.LabelSet{
			model.AddressLabel:  model.LabelValue(ms.ksmAddr),
			model.InstanceLabel: model.LabelValue(ms.ksmAddr),
		})
	}

	alwaysScrapeClassicHistograms := true
	cfg.ScrapeConfigs = append(cfg.ScrapeConfigs, &config.ScrapeConfig{
		JobName:                       jobName,
		HonorLabels:                   true,
		AlwaysScrapeClassicHistograms: &alwaysScrapeClassicHistograms,
		MetricsPath:                   "/metrics",
		Scheme:                        "http",
		EnableCompression:             false,
		ServiceDiscoveryConfigs: []discovery.Config{
			discovery.StaticConfig{{Targets: targets}},
		},
		MetricRelabelConfigs: []*relabel.Config{
			{
				Regex:  relabel.MustNewRegexp("customresource_(group|kind|version)"),
				Action: relabel.LabelDrop,
			},
		},
	})
	if k8sCfg := k8sDiscovery(); k8sCfg != nil {
		klog.Infoln("enabling k8s service discovery")
		cfg.ScrapeConfigs = append(cfg.ScrapeConfigs, k8sCfg)
	}

	// cfg is built by hand above rather than via config.Load()/LoadFile(), so its
	// unexported "loaded" flag is never set. Since Prometheus 0.311, GetScrapeConfigs
	// refuses to run on a config that wasn't loaded that way, so round-trip cfg through
	// Load() (the only public way to set that flag) before using it any further.
	loadedCfg, err := config.Load(cfg.String(), logger)
	if err != nil {
		return err
	}
	cfg = *loadedCfg

	localStorage := &readyStorage{stats: tsdb.NewDBStats()}
	scraper := &readyScrapeManager{}
	remoteStorage := remote.NewStorage(logger, prometheus.DefaultRegisterer, localStorage.StartTime, ms.walDir, RemoteFlushDeadline, scraper, false)
	fanoutStorage := storage.NewFanout(logger, localStorage, remoteStorage)

	if err := remoteStorage.ApplyConfig(&cfg); err != nil {
		return err
	}
	sdMetrics, err := discovery.CreateAndRegisterSDMetrics(prometheus.DefaultRegisterer)
	if err != nil {
		return err
	}
	discMgr := discovery.NewManager(context.TODO(), logger, prometheus.DefaultRegisterer, sdMetrics, discovery.Name("scrape"))
	if discMgr == nil {
		return errors.New("could not create discovery manager")
	}
	c := make(map[string]discovery.Configs)
	scfgs, err := cfg.GetScrapeConfigs()
	if err != nil {
		return err
	}
	for _, v := range scfgs {
		c[v.JobName] = v.ServiceDiscoveryConfigs
	}
	if err = discMgr.ApplyConfig(c); err != nil {
		return err
	}

	go func() {
		if err = discMgr.Run(); err != nil {
			klog.Exitln("error running discovery manager:", err)
		}
	}()

	scrapeManager, err := scrape.NewManager(nil, logger, nil, nil, fanoutStorage, prometheus.DefaultRegisterer)
	if err != nil {
		return err
	}
	if err = scrapeManager.ApplyConfig(&cfg); err != nil {
		return err
	}
	scraper.Set(scrapeManager)
	db, err := agent.Open(logger, prometheus.DefaultRegisterer, remoteStorage, ms.walDir, agent.DefaultOptions())
	if err != nil {
		return err
	}
	localStorage.Set(db, 0)
	db.SetWriteNotified(remoteStorage)
	go func() {
		if err = scrapeManager.Run(discMgr.SyncCh()); err != nil {
			klog.Exitln("error running scrape manager:", err)
		}
	}()
	return nil
}

func k8sDiscovery() *config.ScrapeConfig {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if len(host) == 0 || len(port) == 0 {
		klog.Infoln("not in k8s cluster, disabling k8s service discovery")
		return nil
	}
	alwaysScrapeClassicHistograms := true
	return &config.ScrapeConfig{
		JobName:                       "custom-metrics-k8s-pods",
		HonorLabels:                   true,
		AlwaysScrapeClassicHistograms: &alwaysScrapeClassicHistograms,
		MetricsPath:                   "/metrics",
		Scheme:                        "http",
		EnableCompression:             false,
		RelabelConfigs: []*relabel.Config{
			{
				SourceLabels: model.LabelNames{"__meta_kubernetes_pod_annotation_coroot_com_scrape_metrics"},
				Action:       relabel.Keep,
				Regex:        relabel.MustNewRegexp("true"),
			},
			{
				SourceLabels: model.LabelNames{"__meta_kubernetes_pod_annotation_coroot_com_metrics_path"},
				Action:       relabel.Replace,
				TargetLabel:  "__metrics_path__",
				Regex:        relabel.MustNewRegexp("(.+)"),
				Replacement:  "$1",
			},
			{
				SourceLabels: model.LabelNames{"__meta_kubernetes_pod_name"},
				TargetLabel:  "pod",
				Action:       relabel.Replace,
				Regex:        relabel.MustNewRegexp("(.+)"),
				Replacement:  "$1",
			},
			{
				SourceLabels: model.LabelNames{"__meta_kubernetes_namespace"},
				TargetLabel:  "namespace",
				Action:       relabel.Replace,
				Regex:        relabel.MustNewRegexp("(.+)"),
				Replacement:  "$1",
			},
			{
				SourceLabels: model.LabelNames{"__address__", "__meta_kubernetes_pod_annotation_coroot_com_metrics_port"},
				TargetLabel:  "__address__",
				Separator:    ";",
				Regex:        relabel.MustNewRegexp(`(.+?)(?::\d+)?;(\d+)`),
				Replacement:  "$1:$2",
				Action:       relabel.Replace,
			},
			{
				SourceLabels: model.LabelNames{"__meta_kubernetes_service_annotation_coroot_com_metrics_scheme"},
				TargetLabel:  "__scheme__",
				Regex:        relabel.MustNewRegexp("(https?)"),
				Replacement:  "$1",
				Action:       relabel.Replace,
			},
			{
				SourceLabels: model.LabelNames{"__meta_kubernetes_pod_phase"},
				Regex:        relabel.MustNewRegexp("Pending|Succeeded|Failed|Completed"),
				Action:       relabel.Drop,
			},
		},
		ServiceDiscoveryConfigs: []discovery.Config{
			k8sSDConfig(),
		},
	}
}

func k8sSDConfig() *kubernetes.SDConfig {
	sd := kubernetes.DefaultSDConfig
	sd.Role = kubernetes.RolePod
	sd.Selectors = []kubernetes.SelectorConfig{{Role: "pod"}}
	return &sd
}
