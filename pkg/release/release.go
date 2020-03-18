package release

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/fluxcd/flux/pkg/git"
	"github.com/go-kit/kit/log"

	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/fluxcd/helm-operator/pkg/annotator"
	"github.com/fluxcd/helm-operator/pkg/apis/helm.fluxcd.io/v1"
	"github.com/fluxcd/helm-operator/pkg/chartsync"
	v1client "github.com/fluxcd/helm-operator/pkg/client/clientset/versioned/typed/helm.fluxcd.io/v1"
	"github.com/fluxcd/helm-operator/pkg/helm"
	"github.com/fluxcd/helm-operator/pkg/status"
)

// Config holds the configuration for releases.
type Config struct {
	ChartCache         string
	UpdateDeps         bool
	LogDiffs           bool
	DefaultHelmVersion string
}

// WithDefaults sets the default values for the release config.
func (c Config) WithDefaults() Config {
	if c.ChartCache == "" {
		c.ChartCache = "/tmp"
	}
	return c
}

// Release holds the elements required to perform a Helm release,
// and provides the methods to perform a sync or uninstall.
type Release struct {
	logger       log.Logger
	helmClients  *helm.Clients
	coreV1Client corev1client.CoreV1Interface
	hrClient     v1client.HelmV1Interface
	gitChartSync *chartsync.GitChartSync
	annotator    *annotator.Annotator
	config       Config
}

func New(logger log.Logger, helmClients *helm.Clients, coreV1Client corev1client.CoreV1Interface, hrClient v1client.HelmV1Interface,
	gitChartSync *chartsync.GitChartSync, annotator *annotator.Annotator, config Config) *Release {
	r := &Release{
		logger:       logger,
		helmClients:  helmClients,
		coreV1Client: coreV1Client,
		hrClient:     hrClient,
		gitChartSync: gitChartSync,
		annotator:    annotator,
		config:       config.WithDefaults(),
	}
	return r
}

// Sync synchronizes the given HelmRelease with Helm.
func (r *Release) Sync(hr *v1.HelmRelease) (err error) {
	defer func(start time.Time) {
		ObserveRelease(start, err == nil, hr.GetTargetNamespace(), hr.GetReleaseName())
	}(time.Now())
	defer status.SetObservedGeneration(r.hrClient.HelmReleases(hr.Namespace), hr, hr.Generation)

	client, ok := r.helmClients.Load(hr.GetHelmVersion(r.config.DefaultHelmVersion))
	if !ok {
		status.SetStatusPhase(r.hrClient.HelmReleases(hr.GetTargetNamespace()), hr, v1.HelmReleasePhaseFailed)
		return fmt.Errorf("no client found for Helm '%s'", r.config.DefaultHelmVersion)
	}

	logger := releaseLogger(r.logger, client, hr)
	logger.Log("info", "starting sync run")

	chart, cleanup, err := r.prepareChart(client, hr)
	if err != nil {
		status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, v1.HelmReleasePhaseChartFetchFailed)
		err = fmt.Errorf("failed to prepare chart for release: %w", err)
		logger.Log("error", err)
		return
	}
	if cleanup != nil {
		defer cleanup()
	}
	status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, v1.HelmReleasePhaseChartFetched)

	var values []byte
	values, err = composeValues(r.coreV1Client, hr, chart.chartPath)
	if err != nil {
		status.SetStatusPhase(r.hrClient.HelmReleases(hr.GetTargetNamespace()), hr, v1.HelmReleasePhaseFailed)
		err = fmt.Errorf("failed to compose values for release: %w", err)
		logger.Log("error", err)
		return
	}
	var action action
	var curRel *helm.Release
	action, curRel, err = r.determineSyncAction(client, hr)
	if err != nil {
		status.SetStatusPhase(r.hrClient.HelmReleases(hr.GetTargetNamespace()), hr, v1.HelmReleasePhaseFailed)
		err = fmt.Errorf("failed to determine sync action for release: %w", err)
		logger.Log("error", err)
		return
	}

	return r.run(logger, client, action, hr, curRel, chart, values)
}

// Uninstalls removes the Helm release for the given HelmRelease,
// and the git chart source if present.
func (r *Release) Uninstall(hr *v1.HelmRelease) error {
	client, ok := r.helmClients.Load(hr.GetHelmVersion(r.config.DefaultHelmVersion))
	if !ok {
		return fmt.Errorf(`no client found for Helm "%s"`, r.config.DefaultHelmVersion)
	}
	logger := releaseLogger(r.logger, client, hr)
	return r.run(logger, client, UninstallAction, hr, nil, chart{}, nil)
}

// chart is a reference to a Helm chart used internally during the release.
type chart struct {
	chartPath string
	revision  string
}

// prepareChart returns the chart for the configured chart source in
// the given HelmRelease, or an error.
func (r *Release) prepareChart(client helm.Client, hr *v1.HelmRelease) (chart, func() error, error) {
	var chartPath, revision string
	switch {
	case hr.Spec.GitChartSource != nil && hr.Spec.GitURL != "" && hr.Spec.Path != "":
		var export *git.Export
		var err error

		export, revision, err = r.gitChartSync.GetMirrorCopy(hr)
		if err != nil {
			return chart{}, nil, err
		}
		chartPath = filepath.Join(export.Dir(), hr.Spec.GitChartSource.Path)

		if r.config.UpdateDeps && !hr.Spec.GitChartSource.SkipDepUpdate {
			client.DependencyUpdate(chartPath)
		}
		return chart{chartPath, revision}, export.Clean, nil
	case hr.Spec.RepoChartSource != nil && hr.Spec.RepoURL != "" && hr.Spec.Name != "" && hr.Spec.Version != "":
		var err error

		chartPath, _, err = chartsync.EnsureChartFetched(client, r.config.ChartCache, hr.Spec.RepoChartSource)
		revision = hr.Spec.RepoChartSource.Version
		if err != nil {
			return chart{}, nil, err
		}
	default:
		return chart{}, nil, fmt.Errorf("could not find valid chart source configuration for release")
	}
	return chart{chartPath, revision}, nil, nil
}

type action string

const (
	InstallAction       action = "install"
	UpgradeAction       action = "upgrade"
	SkipAction          action = "skip"
	RollbackAction      action = "rollback"
	UninstallAction     action = "uninstall"
	DryRunCompareAction action = "dry-run-compare"
	AnnotateAction      action = "annotate"
)

// shouldSync determines if the given HelmRelease should be synced
// with Helm. The cheapest checks which do not require a dry-run are
// consulted first (e.g. is this our first sync, have we already seen
// this revision of the resource); before running the dry-run release to
// determine if any undefined mutations have occurred. It returns a
// booleans indicating if the release should be synced, or an error.
func (r *Release) determineSyncAction(client helm.Client, hr *v1.HelmRelease) (action, *helm.Release, error) {
	curRel, err := client.Get(hr.GetReleaseName(), helm.GetOptions{Namespace: hr.GetTargetNamespace()})
	if err != nil {
		return SkipAction, nil, fmt.Errorf("failed to retrieve Helm release: %w", err)
	}

	// If there is no existing release, we should install.
	if curRel == nil {
		return InstallAction, nil, nil
	}

	// Check if the release is managed by our resource, if the release is
	// appears to be managed by another `HelmRelease` resource, or an error
	// is returned, we skip to avoid conflicts.
	managedBy, antecedent, err := r.annotator.OneHasAnnotationWithValueOrNil(curRel.Resources, curRel.Namespace,
		v1.AntecedentAnnotation, hr.ResourceID().String())
	if err != nil {
		return SkipAction, nil, fmt.Errorf("failed to determine ownership over release: %w", err)
	}
	if !managedBy {
		return SkipAction, nil, fmt.Errorf("release appears to be managed by '%s'", antecedent)
	}

	// If the current state of the release does not allow us to safely
	// upgrade, we skip.
	if s := curRel.Info.Status; !s.AllowsUpgrade() {
		return SkipAction, nil, fmt.Errorf("status '%s' of release does not allow a safe upgrade", s.String())
	}

	// If this revision of the `HelmRelease` has not been synchronized
	// yet we attempt an upgrade.
	if !status.HasSynced(hr) {
		return UpgradeAction, curRel, nil
	}

	// The release has been rolled back, inspect state.
	if status.HasRolledBack(hr) {
		if status.ShouldRetryUpgrade(hr) {
			return UpgradeAction, curRel, nil
		}
		hist, err := client.History(hr.GetReleaseName(), helm.HistoryOptions{Namespace: hr.GetTargetNamespace(), Max: hr.GetMaxHistory()})
		if err != nil {
			return SkipAction, nil, fmt.Errorf("failed to retreive history for rolled back release: %w", err)
		}
		for _, r := range hist {
			if r.Info.Status == helm.StatusFailed || r.Info.Status == helm.StatusSuperseded {
				curRel = r
				break
			}
		}
	}
	return DryRunCompareAction, curRel, nil
}

type runErrors []error

func (err runErrors) Error() string {
	var errs []string
	for _, e := range err {
		errs = append(errs, e.Error())
	}
	return strings.Join(errs, ", previous error:")
}

// run starts on the given action and loops through the release cycle.
func (r *Release) run(logger log.Logger, client helm.Client, action action, hr *v1.HelmRelease, curRel *helm.Release,
	chart chart, values []byte) error {

	var newRel *helm.Release
	var errs runErrors
next:
	var err error
	switch action {
	case DryRunCompareAction:
		logger.Log("info", fmt.Sprintf("running dry-run upgrade to compare with release version '%d'", curRel.Version), "action", action)
		var diff string
		newRel, diff, err = r.dryRunCompare(client, curRel, hr, chart, values)
		if err != nil {
			status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, v1.HelmReleasePhaseFailed)
			logger.Log("error", err, "phase", action)
			errs = append(errs, fmt.Errorf("dry-run upgrade failed: %w", err))
			break
		}
		if diff != "" {
			switch r.config.LogDiffs {
			case true:
				logger.Log("info", "difference detected during release comparison", "diff", diff, "phase", action)
			default:
				logger.Log("info", "difference detected during release comparison", "phase", action)
			}
			action = UpgradeAction
			goto next
		}
		logger.Log("info", "no changes", "phase", action)
	case InstallAction:
		logger.Log("info", "running installation", "phase", action)
		newRel, err = r.install(client, hr, chart, values)
		if err != nil {
			logger.Log("error", err, "phase", action)
			errs = append(errs, err)

			action = UninstallAction
			goto next
		}
		logger.Log("info", "installation succeeded", "phase", action)

		action = AnnotateAction
		goto next
	case UpgradeAction:
		logger.Log("info", "running upgrade", "action", action)
		newRel, err = r.upgrade(client, hr, chart, values)
		if err != nil {
			logger.Log("error", err, "action", action)
			errs = append(errs, err)
			if hr.Spec.Rollback.Enable {
				latestRel, err := client.Get(hr.GetReleaseName(), helm.GetOptions{Namespace: hr.GetTargetNamespace(), Version: 0})
				if err != nil {
					err = fmt.Errorf("unable to determine if rollback should be performed: %w", err)
					logger.Log("error", err, "phase", action)
					errs = append(errs, err)
					break
				}
				if curRel.Version < latestRel.Version {
					action = RollbackAction
					goto next
				}
			}
		}
		logger.Log("info", "upgrade succeeded", "phase", action)

		action = AnnotateAction
		goto next
	case RollbackAction:
		logger.Log("info", "running rollback", "phase", action)
		if newRel, err = r.rollback(client, hr); err != nil {
			errs = append(errs, err)
			logger.Log("error", err, "phase", action)
			break
		}
		logger.Log("info", "rollback succeeded", "phase", action)

		action = AnnotateAction
		goto next
	case AnnotateAction:
		if err := annotate(r.annotator, hr, *newRel); err != nil {
			logger.Log("warning", err, "phase", action)
		}
	case UninstallAction:
		if err := uninstall(client, hr); err != nil {
			logger.Log("warning", err, "phase", action)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

// dryRunCompare performs a dry-run upgrade with the given
// `HelmRelease`, chart and values, and  makes a comparison with the
// given release. It returns the dry-run  release and a diff string,
// or an error.
func (r *Release) dryRunCompare(client helm.Client, rel *helm.Release, hr *v1.HelmRelease,
	chart chart, values []byte) (dryRel *helm.Release, diff string, err error) {
	defer func(start time.Time) {
		ObserveReleaseAction(start, DryRunCompareAction, err == nil, hr.GetTargetNamespace(), hr.GetReleaseName())
		if err != nil {
			println(err.Error())
		}
	}(time.Now())
	dryRel, err = client.UpgradeFromPath(chart.chartPath, hr.GetReleaseName(), values, helm.UpgradeOptions{
		DryRun:      true,
		Namespace:   hr.GetTargetNamespace(),
		Force:       hr.Spec.ForceUpgrade,
		ResetValues: hr.Spec.ResetValues,
	})
	if err != nil {
		err = fmt.Errorf("dry-run upgrade for comparison failed: %w", err)
		return
	}
	diff = helm.Diff(rel, dryRel)
	return
}

// install performs an installation with the given HelmRelease,
// chart, and values while recording the phases on the HelmRelease.
// It returns the release result or an error.
func (r *Release) install(client helm.Client, hr *v1.HelmRelease, chart chart, values []byte) (rel *helm.Release, err error) {
	defer func(start time.Time) {
		ObserveReleaseAction(start, InstallAction, err == nil, hr.GetTargetNamespace(), hr.GetReleaseName())
	}(time.Now())
	status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, v1.HelmReleasePhaseInstalling)
	rel, err = client.UpgradeFromPath(chart.chartPath, hr.GetReleaseName(), values, helm.UpgradeOptions{
		Namespace:  hr.GetTargetNamespace(),
		Timeout:    hr.GetTimeout(),
		Install:    true,
		Force:      hr.Spec.ForceUpgrade,
		SkipCRDs:   hr.Spec.SkipCRDs,
		MaxHistory: hr.GetMaxHistory(),
		Wait:       hr.Spec.Wait,
	})
	if err != nil {
		status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, v1.HelmReleasePhaseFailed)
		err = fmt.Errorf("installation failed: %w", err)
		return
	}
	status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, v1.HelmReleasePhaseSucceeded)
	status.SetReleaseRevision(r.hrClient.HelmReleases(hr.Namespace), hr, chart.revision)
	return
}

// upgrade performs an upgrade with the given HelmRelease,
// chart and values while recording the phases and revision on
// the HelmRelease. It returns the release result or an error.
func (r *Release) upgrade(client helm.Client, hr *v1.HelmRelease, chart chart, values []byte) (rel *helm.Release, err error) {
	defer func(start time.Time) {
		ObserveReleaseAction(start, UpgradeAction, err == nil, hr.GetTargetNamespace(), hr.GetReleaseName())
	}(time.Now())
	status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, v1.HelmReleasePhaseUpgrading)
	rel, err = client.UpgradeFromPath(chart.chartPath, hr.GetReleaseName(), values, helm.UpgradeOptions{
		Namespace:   hr.GetTargetNamespace(),
		Timeout:     hr.GetTimeout(),
		Install:     false,
		Force:       hr.Spec.ForceUpgrade,
		ResetValues: hr.Spec.ResetValues,
		SkipCRDs:    hr.Spec.SkipCRDs,
		MaxHistory:  hr.GetMaxHistory(),
		Wait:        hr.Spec.Wait || hr.Spec.Rollback.Enable,
	})
	if err != nil {
		status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, v1.HelmReleasePhaseFailed)
		err = fmt.Errorf("upgrade failed: %w", err)
		return
	}
	status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, v1.HelmReleasePhaseSucceeded)
	status.SetReleaseRevision(r.hrClient.HelmReleases(hr.Namespace), hr, chart.revision)
	return
}

// rollback performs an upgrade for the given HelmRelease,
// while recording the phases on  the HelmRelease. It returns
// the release result or an error.
func (r *Release) rollback(client helm.Client, hr *v1.HelmRelease) (rel *helm.Release, err error) {
	defer func(start time.Time) {
		ObserveReleaseAction(start, RollbackAction, err == nil, hr.GetTargetNamespace(), hr.GetReleaseName())
	}(time.Now())
	status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, v1.HelmReleasePhaseRollingBack)
	rel, err = client.Rollback(hr.GetReleaseName(), helm.RollbackOptions{
		Namespace:    hr.GetTargetNamespace(),
		Timeout:      hr.Spec.Rollback.GetTimeout(),
		Wait:         hr.Spec.Rollback.Wait,
		DisableHooks: hr.Spec.Rollback.DisableHooks,
		Recreate:     hr.Spec.Rollback.Recreate,
		Force:        hr.Spec.Rollback.Force,
	})
	if err != nil {
		status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, v1.HelmReleasePhaseRollbackFailed)
		err = fmt.Errorf("rollback failed: %w", err)
		return
	}
	status.SetStatusPhase(r.hrClient.HelmReleases(hr.Namespace), hr, v1.HelmReleasePhaseRolledBack)
	return
}

// annotate annotates the given release resources on the cluster with
// the resource ID of the given HelmRelease.
func annotate(annotator *annotator.Annotator, hr *v1.HelmRelease, rel helm.Release) (err error) {
	defer func(start time.Time) {
		ObserveReleaseAction(start, AnnotateAction, err == nil, hr.GetTargetNamespace(), hr.GetReleaseName())
	}(time.Now())
	err = annotator.Annotate(rel.Resources, hr.GetTargetNamespace(), v1.AntecedentAnnotation, hr.ResourceID().String())
	if err != nil {
		err = fmt.Errorf("failed to annotate release resources: %w", err)
	}
	return
}

func uninstall(client helm.Client, hr *v1.HelmRelease) (err error) {
	defer func(start time.Time) {
		ObserveReleaseAction(start, UninstallAction, err == nil, hr.GetTargetNamespace(), hr.GetReleaseName())
	}(time.Now())
	err = client.Uninstall(hr.GetReleaseName(), helm.UninstallOptions{
		Namespace:   hr.GetTargetNamespace(),
		KeepHistory: false,
		Timeout:     hr.GetTimeout(),
	})
	if err != nil {
		err = fmt.Errorf("uninstall failed: %w", err)
	}
	return
}

// releaseLogger returns a logger in the context of the given
// HelmRelease (that being, with metadata included).
func releaseLogger(logger log.Logger, client helm.Client, hr *v1.HelmRelease) log.Logger {
	return log.With(logger,
		"release", hr.GetReleaseName(),
		"targetNamespace", hr.GetTargetNamespace(),
		"resource", hr.ResourceID().String(),
		"helmVersion", client.Version(),
	)
}
