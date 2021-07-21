package controlplane

import (
	"context"
	"github.com/maistra/istio-operator/pkg/controller/common"
	"github.com/maistra/istio-operator/pkg/controller/common/helm"
)

func (r *controlPlaneInstanceReconciler) processComponentManifests(ctx context.Context, chartName string, isExternalProfileActive bool) (hasReadiness bool, err error) {
	componentName := componentFromChartName(chartName)
	log := common.LogFromContext(ctx).WithValues("Component", componentName)
	ctx = common.NewContextWithLog(ctx, log)

	renderings, hasRenderings := r.renderings[chartName]
	if !hasRenderings {
		log.V(5).Info("no renderings for component")
		return false, nil
	}

	log.Info("reconciling component resources")
	status := r.Status.FindComponentByName(componentName)
	defer func() {
		updateComponentConditions(status, err)
		log.Info("component reconciliation complete")
	}()

	tmpCR := r.ControllerResources
	tmpCR.Client = r.controlPlaneSecondaryKubeClient

	var mp *helm.ManifestProcessor

	if isExternalProfileActive {
		if chartName == "telemetry-common" || chartName == "istio-discovery" {
			mp = helm.NewManifestProcessor(tmpCR, helm.NewPatchFactory(r.controlPlaneSecondaryKubeClient), r.Instance.GetNamespace(), r.meshGeneration, r.Instance.GetNamespace(), r.preprocessObject, r.processNewObject)
		}
	} else {
		mp = helm.NewManifestProcessor(r.ControllerResources, helm.NewPatchFactory(r.Client), r.Instance.GetNamespace(), r.meshGeneration, r.Instance.GetNamespace(), r.preprocessObject, r.processNewObject)
	}

	log.Info("XXX new Manifest Processor has been created, started manifest processing")
	if err = mp.ProcessManifests(ctx, renderings, status.Resource); err != nil {
		return false, err
	}
	if err = r.processNewComponent(componentName, status); err != nil {
		log.Error(err, "error during postprocessing of component")
		return false, err
	}

	// if we get here, the component has been successfully installed
	delete(r.renderings, chartName)

	for _, rendering := range renderings {
		if r.hasReadiness(rendering.Head.Kind) {
			return true, nil
		}
	}
	return false, nil
}
