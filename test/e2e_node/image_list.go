/*
Copyright 2016 The Kubernetes Authors.

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

package e2enode

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"time"

	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/util/sets"
	internalapi "k8s.io/cri-api/pkg/apis"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	commontest "k8s.io/kubernetes/test/e2e/common"
	"k8s.io/kubernetes/test/e2e/framework"
	e2egpu "k8s.io/kubernetes/test/e2e/framework/gpu"
	e2emanifest "k8s.io/kubernetes/test/e2e/framework/manifest"
	e2etestfiles "k8s.io/kubernetes/test/e2e/framework/testfiles"
	imageutils "k8s.io/kubernetes/test/utils/image"
)

const (
	// Number of attempts to pull an image.
	maxImagePullRetries = 5
	// Sleep duration between image pull retry attempts.
	imagePullRetryDelay = time.Second
)

// NodePrePullImageList is a list of images used in node e2e test. These images will be prepulled
// before test running so that the image pulling won't fail in actual test.
var NodePrePullImageList = sets.NewString(
	imageutils.GetE2EImage(imageutils.Agnhost),
	"google/cadvisor:latest",
	"k8s.gcr.io/stress:v1",
	busyboxImage,
	"k8s.gcr.io/busybox@sha256:4bdd623e848417d96127e16037743f0cd8b528c026e9175e22a84f639eca58ff",
	imageutils.GetE2EImage(imageutils.Nginx),
	imageutils.GetE2EImage(imageutils.Perl),
	imageutils.GetE2EImage(imageutils.Nonewprivs),
	imageutils.GetPauseImageName(),
	getGPUDevicePluginImage(),
	"gcr.io/kubernetes-e2e-test-images/node-perf/npb-is:1.0",
	"gcr.io/kubernetes-e2e-test-images/node-perf/npb-ep:1.0",
	"gcr.io/kubernetes-e2e-test-images/node-perf/tf-wide-deep-amd64:1.0",
)

// updateImageAllowList updates the framework.ImagePrePullList with
// 1. the hard coded lists
// 2. the ones passed in from framework.TestContext.ExtraEnvs
// So this function needs to be called after the extra envs are applied.
func updateImageAllowList() {
	// Union NodePrePullImageList and PrePulledImages into the framework image pre-pull list.
	framework.ImagePrePullList = NodePrePullImageList.Union(commontest.PrePulledImages)
	// Images from extra envs
	framework.ImagePrePullList.Insert(getNodeProblemDetectorImage())
	framework.ImagePrePullList.Insert(getSRIOVDevicePluginImage())
}

func getNodeProblemDetectorImage() string {
	const defaultImage string = "k8s.gcr.io/node-problem-detector:v0.6.2"
	image := os.Getenv("NODE_PROBLEM_DETECTOR_IMAGE")
	if image == "" {
		image = defaultImage
	}
	return image
}

// puller represents a generic image puller
type puller interface {
	// Pull pulls an image by name
	Pull(image string) ([]byte, error)
	// Name returns the name of the specific puller implementation
	Name() string
}

type dockerPuller struct {
}

func (dp *dockerPuller) Name() string {
	return "docker"
}

func (dp *dockerPuller) Pull(image string) ([]byte, error) {
	// TODO(random-liu): Use docker client to get rid of docker binary dependency.
	if exec.Command("docker", "inspect", "--type=image", image).Run() != nil {
		return exec.Command("docker", "pull", image).CombinedOutput()
	}
	return nil, nil
}

type remotePuller struct {
	imageService internalapi.ImageManagerService
}

func (rp *remotePuller) Name() string {
	return "CRI"
}

func (rp *remotePuller) Pull(image string) ([]byte, error) {
	imageStatus, err := rp.imageService.ImageStatus(&runtimeapi.ImageSpec{Image: image})
	if err == nil && imageStatus != nil {
		return nil, nil
	}
	_, err = rp.imageService.PullImage(&runtimeapi.ImageSpec{Image: image}, nil, nil)
	return nil, err
}

func getPuller() (puller, error) {
	runtime := framework.TestContext.ContainerRuntime
	switch runtime {
	case "docker":
		return &dockerPuller{}, nil
	case "remote":
		_, is, err := getCRIClient()
		if err != nil {
			return nil, err
		}
		return &remotePuller{
			imageService: is,
		}, nil
	}
	return nil, fmt.Errorf("can't prepull images, unknown container runtime %q", runtime)
}

// PrePullAllImages pre-fetches all images tests depend on so that we don't fail in an actual test.
func PrePullAllImages() error {
	puller, err := getPuller()
	if err != nil {
		return err
	}
	usr, err := user.Current()
	if err != nil {
		return err
	}
	images := framework.ImagePrePullList.List()
	klog.V(4).Infof("Pre-pulling images with %s %+v", puller.Name(), images)
	for _, image := range images {
		var (
			err    error
			output []byte
		)
		for i := 0; i < maxImagePullRetries; i++ {
			if i > 0 {
				time.Sleep(imagePullRetryDelay)
			}
			if output, err = puller.Pull(image); err == nil {
				break
			}
			klog.Warningf("Failed to pull %s as user %q, retrying in %s (%d of %d): %v",
				image, usr.Username, imagePullRetryDelay.String(), i+1, maxImagePullRetries, err)
		}
		if err != nil {
			klog.Warningf("Could not pre-pull image %s %v output: %s", image, err, output)
			return err
		}
	}
	return nil
}

// getGPUDevicePluginImage returns the image of GPU device plugin.
func getGPUDevicePluginImage() string {
	ds, err := e2emanifest.DaemonSetFromURL(e2egpu.GPUDevicePluginDSYAML)
	if err != nil {
		klog.Errorf("Failed to parse the device plugin image: %v", err)
		return ""
	}
	if ds == nil {
		klog.Errorf("Failed to parse the device plugin image: the extracted DaemonSet is nil")
		return ""
	}
	if len(ds.Spec.Template.Spec.Containers) < 1 {
		klog.Errorf("Failed to parse the device plugin image: cannot extract the container from YAML")
		return ""
	}
	return ds.Spec.Template.Spec.Containers[0].Image
}

// getSRIOVDevicePluginImage returns the image of SRIOV device plugin.
func getSRIOVDevicePluginImage() string {
	data, err := e2etestfiles.Read(SRIOVDevicePluginDSYAML)
	if err != nil {
		klog.Errorf("Failed to read the device plugin manifest: %v", err)
		return ""
	}
	ds, err := e2emanifest.DaemonSetFromData(data)
	if err != nil {
		klog.Errorf("Failed to parse the device plugin image: %v", err)
		return ""
	}
	if ds == nil {
		klog.Errorf("Failed to parse the device plugin image: the extracted DaemonSet is nil")
		return ""
	}
	if len(ds.Spec.Template.Spec.Containers) < 1 {
		klog.Errorf("Failed to parse the device plugin image: cannot extract the container from YAML")
		return ""
	}
	return ds.Spec.Template.Spec.Containers[0].Image
}
