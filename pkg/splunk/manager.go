package splunk

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	enterpriseApi "github.com/splunk/splunk-operator/api/v4"
	gateway "github.com/splunk/splunk-operator/pkg/gateway/splunk/services"
	splcommon "github.com/splunk/splunk-operator/pkg/splunk/common"
	types "github.com/splunk/splunk-operator/pkg/splunk/model"
)

type Factory interface {
	NewManager(ctx context.Context, info *types.ReconcileInfo, publisher gateway.EventPublisher) (SplunkManager, error)
}

type SplunkManager interface {
	ApplyClusterManager(ctx context.Context, client splcommon.ControllerClient, cr *enterpriseApi.ClusterManager) (reconcile.Result, error)
	//ApplyClusterMaster(ctx context.Context, cr *enterpriseApiV3.ClusterMaster) (reconcile.Result, error)
	//ApplyIndexerClusterManager(ctx context.Context,  cr *enterpriseApi.IndexerCluster) (reconcile.Result, error)
	//ApplyMonitoringConsole(ctx context.Context,  cr *enterpriseApi.MonitoringConsole) (reconcile.Result, error)
	//ApplySearchHeadCluster(ctx context.Context,  cr *enterpriseApi.SearchHeadCluster) (reconcile.Result, error)
	//ApplyStandalone(ctx context.Context,  cr *enterpriseApi.Standalone) (reconcile.Result, error)
	//ApplyLicenseManager(ctx context.Context,  cr *enterpriseApi.LicenseManager) (reconcile.Result, error)
	//ApplyLicenseMaster(ctx context.Context,  cr *enterpriseApiV3.LicenseMaster) (reconcile.Result, error)
}
