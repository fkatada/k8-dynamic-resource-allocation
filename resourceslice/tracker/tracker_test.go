/*
Copyright 2025 The Kubernetes Authors.

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

package tracker

import (
	stdcmp "cmp"
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	resourcealphaapi "k8s.io/api/resource/v1alpha3"
	resourceapi "k8s.io/api/resource/v1beta1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/ktesting"
	_ "k8s.io/klog/v2/ktesting/init"
	"k8s.io/utils/ptr"
)

type handlerEventType string

const (
	handlerEventAdd    handlerEventType = "add"
	handlerEventUpdate handlerEventType = "update"
	handlerEventDelete handlerEventType = "delete"
)

type handlerEvent struct {
	event  handlerEventType
	oldObj *resourceapi.ResourceSlice
	newObj *resourceapi.ResourceSlice
}

type inputEventGenerator struct {
	addResourceSlice      func(slice *resourceapi.ResourceSlice)
	deleteResourceSlice   func(name string)
	addDeviceTaintRule    func(taintRule *resourcealphaapi.DeviceTaintRule)
	deleteDeviceTaintRule func(name string)
	addDeviceClass        func(class *resourceapi.DeviceClass)
	deleteDeviceClass     func(name string)
}

func inputEventGeneratorForTest(ctx context.Context, t *testing.T, tracker *Tracker) inputEventGenerator {
	return inputEventGenerator{
		addResourceSlice: func(slice *resourceapi.ResourceSlice) {
			oldObj, exists, err := tracker.resourceSlices.GetIndexer().Get(slice)
			require.NoError(t, err)
			err = tracker.resourceSlices.GetIndexer().Add(slice)
			require.NoError(t, err)
			if !exists {
				tracker.resourceSliceAdd(ctx)(slice)
			} else if !apiequality.Semantic.DeepEqual(oldObj, slice) {
				tracker.resourceSliceUpdate(ctx)(oldObj, slice)
			}
		},
		deleteResourceSlice: func(name string) {
			oldObj, exists, err := tracker.resourceSlices.GetIndexer().GetByKey(name)
			require.NoError(t, err)
			require.True(t, exists, "deleting resource slice that was never created")
			err = tracker.resourceSlices.GetIndexer().Delete(oldObj)
			require.NoError(t, err)
			tracker.resourceSliceDelete(ctx)(oldObj)
		},
		addDeviceTaintRule: func(taintRule *resourcealphaapi.DeviceTaintRule) {
			oldObj, exists, err := tracker.deviceTaints.GetIndexer().Get(taintRule)
			require.NoError(t, err)
			err = tracker.deviceTaints.GetIndexer().Add(taintRule)
			require.NoError(t, err)
			if !exists {
				tracker.deviceTaintAdd(ctx)(taintRule)
			} else if !apiequality.Semantic.DeepEqual(oldObj, taintRule) {
				tracker.deviceTaintUpdate(ctx)(oldObj, taintRule)
			}
		},
		deleteDeviceTaintRule: func(name string) {
			oldObj, exists, err := tracker.deviceTaints.GetIndexer().GetByKey(name)
			require.NoError(t, err)
			require.True(t, exists, "deleting DeviceTaintRule that was never created")
			err = tracker.deviceTaints.GetIndexer().Delete(oldObj)
			require.NoError(t, err)
			tracker.deviceTaintDelete(ctx)(oldObj)
		},
		addDeviceClass: func(class *resourceapi.DeviceClass) {
			oldObj, exists, err := tracker.deviceClasses.GetIndexer().Get(class)
			require.NoError(t, err)
			err = tracker.deviceClasses.GetIndexer().Add(class)
			require.NoError(t, err)
			if !exists {
				tracker.deviceClassAdd(ctx)(class)
			} else if !apiequality.Semantic.DeepEqual(oldObj, class) {
				tracker.deviceClassUpdate(ctx)(oldObj, class)
			}
		},
		deleteDeviceClass: func(name string) {
			oldObj, exists, err := tracker.deviceClasses.GetIndexer().GetByKey(name)
			require.NoError(t, err)
			require.True(t, exists, "deleting device class that was never created")
			err = tracker.deviceClasses.GetIndexer().Delete(oldObj)
			require.NoError(t, err)
			tracker.deviceClassDelete(ctx)(oldObj)
		},
	}
}

var (
	now, _      = time.Parse(time.RFC3339, "2006-01-02T15:04:05Z")
	driver1     = "driver1.example.com"
	driver2     = "driver2.example.com"
	pool1       = "pool-1"
	pool2       = "pool-2"
	device0Name = "device-0"
	device1Name = "device-1"
	device2Name = "device-2"

	deviceClass1 = &resourceapi.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{Name: "device-class-1"},
		Spec: resourceapi.DeviceClassSpec{
			Selectors: []resourceapi.DeviceSelector{
				{
					CEL: &resourceapi.CELDeviceSelector{
						Expression: `device.driver == "` + driver1 + `"`,
					},
				},
			},
		},
	}

	sliceWithDevices = func(slice *resourceapi.ResourceSlice, devices []resourceapi.Device) *resourceapi.ResourceSlice {
		slice = slice.DeepCopy()
		slice.Spec.Devices = devices
		return slice
	}
	slice1NoDevices = &resourceapi.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: "s1",
		},
		Spec: resourceapi.ResourceSliceSpec{
			Driver: driver1,
			Pool: resourceapi.ResourcePool{
				Name: pool1,
			},
		},
	}
	slice2NoDevices = &resourceapi.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: "s2",
		},
		Spec: resourceapi.ResourceSliceSpec{
			Driver: driver2,
			Pool: resourceapi.ResourcePool{
				Name: pool2,
			},
		},
	}
	unchangedSlice = &resourceapi.ResourceSlice{ObjectMeta: metav1.ObjectMeta{Name: "no-change"}}

	deviceWithName = func(device resourceapi.Device, name string) resourceapi.Device {
		device.Name = name
		return device
	}
	deviceWithTaints = func(device resourceapi.Device, taints []resourceapi.DeviceTaint) resourceapi.Device {
		if device.Basic != nil {
			device.Basic = device.Basic.DeepCopy()
			device.Basic.Taints = taints
		}
		return device
	}
	emptyDevice = resourceapi.Device{Basic: &resourceapi.BasicDevice{}}
	device0     = deviceWithName(emptyDevice, device0Name)
	device1     = deviceWithName(emptyDevice, device1Name)
	device2     = deviceWithName(emptyDevice, device2Name)

	deviceTaint1 = resourceapi.DeviceTaint{
		Key:       "example.com/taint",
		Value:     "tainted",
		Effect:    resourceapi.DeviceTaintEffectNoExecute,
		TimeAdded: &metav1.Time{Time: now},
	}
	deviceTaint2 = resourceapi.DeviceTaint{
		Key:       "example.com/taint2",
		Value:     "tainted2",
		Effect:    resourceapi.DeviceTaintEffectNoExecute,
		TimeAdded: &metav1.Time{Time: now},
	}
	deviceTaints   = []resourceapi.DeviceTaint{deviceTaint1}
	device1Tainted = deviceWithTaints(device1, deviceTaints)
	device2Tainted = deviceWithTaints(device2, deviceTaints)
	devices        = []resourceapi.Device{device1}
	threeDevices   = []resourceapi.Device{
		device0,
		device1,
		device2,
	}
	threeDevicesOneTainted = []resourceapi.Device{
		device0,
		device1Tainted,
		device2,
	}
	devices2        = []resourceapi.Device{device2}
	taintedDevices  = []resourceapi.Device{device1Tainted}
	taintedDevices2 = []resourceapi.Device{device2Tainted}

	existingDeviceTaints   = []resourceapi.DeviceTaint{deviceTaint2}
	existingDevice1Tainted = deviceWithTaints(device1, existingDeviceTaints)
	existingTaintedDevices = []resourceapi.Device{existingDevice1Tainted}
	mergedDeviceTaints     = []resourceapi.DeviceTaint{deviceTaint2, deviceTaint1}
	mergedDevice1Tainted   = deviceWithTaints(device1, mergedDeviceTaints)
	mergedTaintedDevices   = []resourceapi.Device{mergedDevice1Tainted}

	slice1               = sliceWithDevices(slice1NoDevices, devices)
	slice1Tainted        = sliceWithDevices(slice1, taintedDevices)
	slice1AlreadyTainted = sliceWithDevices(slice1, existingTaintedDevices)
	slice1MergedTaints   = sliceWithDevices(slice1, mergedTaintedDevices)
	slice2               = sliceWithDevices(slice2NoDevices, devices2)
	slice2Tainted        = sliceWithDevices(slice2, taintedDevices2)

	alphaDeviceTaint = func(taint resourceapi.DeviceTaint) resourcealphaapi.DeviceTaint {
		return resourcealphaapi.DeviceTaint{
			Key:       taint.Key,
			Value:     taint.Value,
			Effect:    resourcealphaapi.DeviceTaintEffect(taint.Effect),
			TimeAdded: taint.TimeAdded,
		}
	}
	taintAllDevicesRule = &resourcealphaapi.DeviceTaintRule{
		ObjectMeta: metav1.ObjectMeta{
			Name: "rule",
		},
		Spec: resourcealphaapi.DeviceTaintRuleSpec{
			Taint: alphaDeviceTaint(deviceTaint1),
		},
	}
	taintPoolDevicesRule = func(rule *resourcealphaapi.DeviceTaintRule, pool string) *resourcealphaapi.DeviceTaintRule {
		rule = rule.DeepCopy()
		rule.Spec.DeviceSelector = &resourcealphaapi.DeviceTaintSelector{
			Pool: &pool,
		}
		return rule
	}
	taintDriverDevicesRule = func(rule *resourcealphaapi.DeviceTaintRule, driver string) *resourcealphaapi.DeviceTaintRule {
		rule = rule.DeepCopy()
		rule.Spec.DeviceSelector = &resourcealphaapi.DeviceTaintSelector{
			Driver: &driver,
		}
		return rule
	}
	taintNamedDevicesRule = func(rule *resourcealphaapi.DeviceTaintRule, name string) *resourcealphaapi.DeviceTaintRule {
		rule = rule.DeepCopy()
		rule.Spec.DeviceSelector = &resourcealphaapi.DeviceTaintSelector{
			Device: &name,
		}
		return rule
	}
	taintCELSelectedDevicesRule = func(rule *resourcealphaapi.DeviceTaintRule, exprs ...string) *resourcealphaapi.DeviceTaintRule {
		rule = rule.DeepCopy()
		var selectors []resourcealphaapi.DeviceSelector
		for _, expr := range exprs {
			selectors = append(selectors, resourcealphaapi.DeviceSelector{
				CEL: &resourcealphaapi.CELDeviceSelector{
					Expression: expr,
				},
			})
		}
		rule.Spec.DeviceSelector = &resourcealphaapi.DeviceTaintSelector{
			Selectors: selectors,
		}
		return rule
	}
	taintDeviceClassRule = func(rule *resourcealphaapi.DeviceTaintRule, deviceClassName string) *resourcealphaapi.DeviceTaintRule {
		rule = rule.DeepCopy()
		rule.Spec.DeviceSelector = &resourcealphaapi.DeviceTaintSelector{
			DeviceClassName: &deviceClassName,
		}
		return rule
	}
	taintPool1DevicesRule             = taintPoolDevicesRule(taintAllDevicesRule, pool1)
	taintPool2DevicesRule             = taintPoolDevicesRule(taintAllDevicesRule, pool2)
	taintDriver1DevicesRule           = taintDriverDevicesRule(taintAllDevicesRule, driver1)
	taintDevice1Rule                  = taintNamedDevicesRule(taintAllDevicesRule, device1Name)
	taintDriver1DevicesCELRule        = taintCELSelectedDevicesRule(taintAllDevicesRule, `device.driver == "`+driver1+`"`)
	taintNoDevicesCELRule             = taintCELSelectedDevicesRule(taintAllDevicesRule, `true`, `false`, `true`)
	taintNoDevicesCELRuntimeErrorRule = taintCELSelectedDevicesRule(taintAllDevicesRule, `device.attributes["test.example.com"].deviceAttr`)
	taintNoDevicesInvalidCELRule      = taintCELSelectedDevicesRule(taintAllDevicesRule, `invalid`)
	taintDeviceClass1Rule             = taintDeviceClassRule(taintAllDevicesRule, deviceClass1.Name)
)

func TestListPatchedResourceSlices(t *testing.T) {
	tests := map[string]struct {
		deviceTaintsDisabled  bool
		inputEvents           func(event inputEventGenerator)
		expectedPatchedSlices []*resourceapi.ResourceSlice
		expectHandlerEvents   func(t *testing.T, events []handlerEvent)
		expectEvents          func(t *assert.CollectT, events *v1.EventList)
		expectUnhandledErrors func(t *testing.T, errs []error)
	}{
		"add-slices-no-patches": {
			inputEvents: func(event inputEventGenerator) {
				event.addResourceSlice(slice1)
				event.addResourceSlice(slice2)
			},
			expectedPatchedSlices: []*resourceapi.ResourceSlice{
				slice1,
				slice2,
			},
			expectHandlerEvents: func(t *testing.T, events []handlerEvent) {
				assert.Equal(
					t,
					[]handlerEvent{
						{event: handlerEventAdd, newObj: slice1},
						{event: handlerEventAdd, newObj: slice2},
					},
					events,
				)
			},
		},
		"update-slices-no-patches": {
			inputEvents: func(event inputEventGenerator) {
				event.addResourceSlice(slice1NoDevices)
				event.addResourceSlice(slice2NoDevices)
				event.addResourceSlice(unchangedSlice)

				event.addResourceSlice(slice1)
				event.addResourceSlice(slice2)
				event.addResourceSlice(unchangedSlice)
			},
			expectedPatchedSlices: []*resourceapi.ResourceSlice{
				slice1,
				slice2,
				unchangedSlice,
			},
			expectHandlerEvents: func(t *testing.T, events []handlerEvent) {
				assert.Equal(
					t,
					[]handlerEvent{
						{event: handlerEventAdd, newObj: slice1NoDevices},
						{event: handlerEventAdd, newObj: slice2NoDevices},
						{event: handlerEventAdd, newObj: unchangedSlice},
						{event: handlerEventUpdate, oldObj: slice1NoDevices, newObj: slice1},
						{event: handlerEventUpdate, oldObj: slice2NoDevices, newObj: slice2},
					},
					events,
				)
			},
		},
		"delete-slices": {
			inputEvents: func(event inputEventGenerator) {
				event.addResourceSlice(slice1)
				event.addResourceSlice(slice2)
				event.addResourceSlice(unchangedSlice)
				event.deleteResourceSlice(slice1.Name)
				event.deleteResourceSlice(slice2.Name)
			},
			expectedPatchedSlices: []*resourceapi.ResourceSlice{
				unchangedSlice,
			},
			expectHandlerEvents: func(t *testing.T, events []handlerEvent) {
				assert.Equal(
					t,
					[]handlerEvent{
						{event: handlerEventAdd, newObj: slice1},
						{event: handlerEventAdd, newObj: slice2},
						{event: handlerEventAdd, newObj: unchangedSlice},
						{event: handlerEventDelete, oldObj: slice1},
						{event: handlerEventDelete, oldObj: slice2},
					},
					events,
				)
			},
		},
		"patch-all-slices": {
			inputEvents: func(event inputEventGenerator) {
				event.addResourceSlice(slice1)
				event.addDeviceTaintRule(taintAllDevicesRule)
			},
			expectedPatchedSlices: []*resourceapi.ResourceSlice{
				slice1Tainted,
			},
			expectHandlerEvents: func(t *testing.T, events []handlerEvent) {
				assert.Equal(
					t,
					[]handlerEvent{
						{event: handlerEventAdd, newObj: slice1},
						{event: handlerEventUpdate, oldObj: slice1, newObj: slice1Tainted},
					},
					events,
				)
			},
		},
		"update-patch": {
			inputEvents: func(event inputEventGenerator) {
				event.addDeviceTaintRule(taintPool1DevicesRule)
				event.addResourceSlice(slice1)
				event.addResourceSlice(slice2)
				event.addDeviceTaintRule(taintPool2DevicesRule)
			},
			expectedPatchedSlices: []*resourceapi.ResourceSlice{
				slice1,
				slice2Tainted,
			},
			expectHandlerEvents: func(t *testing.T, events []handlerEvent) {
				assert.Equal(
					t,
					[]handlerEvent{
						{event: handlerEventAdd, newObj: slice1Tainted},
						{event: handlerEventAdd, newObj: slice2},
					},
					events[:2],
				)
				// The remaining events may come in any order
				assert.ElementsMatch(
					t,
					[]handlerEvent{
						{event: handlerEventUpdate, oldObj: slice1Tainted, newObj: slice1},
						{event: handlerEventUpdate, oldObj: slice2, newObj: slice2Tainted},
					},
					events[2:],
				)
			},
		},
		"merge-taints": {
			inputEvents: func(event inputEventGenerator) {
				event.addDeviceTaintRule(taintAllDevicesRule)
				event.addResourceSlice(slice1AlreadyTainted)
			},
			expectedPatchedSlices: []*resourceapi.ResourceSlice{
				slice1MergedTaints,
			},
			expectHandlerEvents: func(t *testing.T, events []handlerEvent) {
				assert.Equal(
					t,
					[]handlerEvent{
						{event: handlerEventAdd, newObj: slice1MergedTaints},
					},
					events,
				)
			},
		},
		"add-taint-for-driver": {
			inputEvents: func(event inputEventGenerator) {
				event.addDeviceTaintRule(taintDriver1DevicesRule)
				event.addResourceSlice(slice1)
				event.addResourceSlice(slice2)
			},
			expectedPatchedSlices: []*resourceapi.ResourceSlice{
				slice1Tainted,
				slice2,
			},
			expectHandlerEvents: func(t *testing.T, events []handlerEvent) {
				assert.Equal(
					t,
					[]handlerEvent{
						{event: handlerEventAdd, newObj: slice1Tainted},
						{event: handlerEventAdd, newObj: slice2},
					},
					events,
				)
			},
		},
		"add-taint-for-pool": {
			inputEvents: func(event inputEventGenerator) {
				event.addDeviceTaintRule(taintPool1DevicesRule)
				event.addResourceSlice(slice1)
				event.addResourceSlice(slice2)
			},
			expectedPatchedSlices: []*resourceapi.ResourceSlice{
				slice1Tainted,
				slice2,
			},
			expectHandlerEvents: func(t *testing.T, events []handlerEvent) {
				assert.Equal(
					t,
					[]handlerEvent{
						{event: handlerEventAdd, newObj: slice1Tainted},
						{event: handlerEventAdd, newObj: slice2},
					},
					events,
				)
			},
		},
		"add-taint-for-device": {
			inputEvents: func(event inputEventGenerator) {
				event.addDeviceTaintRule(taintDevice1Rule)
				event.addResourceSlice(slice1)
				event.addResourceSlice(slice2)
			},
			expectedPatchedSlices: []*resourceapi.ResourceSlice{
				slice1Tainted,
				slice2,
			},
			expectHandlerEvents: func(t *testing.T, events []handlerEvent) {
				assert.Equal(
					t,
					[]handlerEvent{
						{event: handlerEventAdd, newObj: slice1Tainted},
						{event: handlerEventAdd, newObj: slice2},
					},
					events,
				)
			},
		},
		"add-attribute-for-selector": {
			inputEvents: func(event inputEventGenerator) {
				event.addDeviceTaintRule(taintDriver1DevicesCELRule)
				event.addResourceSlice(slice1)
				event.addResourceSlice(slice2)
			},
			expectedPatchedSlices: []*resourceapi.ResourceSlice{
				slice1Tainted,
				slice2,
			},
			expectHandlerEvents: func(t *testing.T, events []handlerEvent) {
				assert.Equal(
					t,
					[]handlerEvent{
						{event: handlerEventAdd, newObj: slice1Tainted},
						{event: handlerEventAdd, newObj: slice2},
					},
					events,
				)
			},
		},
		"selector-does-not-match": {
			inputEvents: func(event inputEventGenerator) {
				event.addDeviceTaintRule(taintNoDevicesCELRule)
				event.addResourceSlice(slice1)
			},
			expectedPatchedSlices: []*resourceapi.ResourceSlice{
				slice1,
			},
			expectHandlerEvents: func(t *testing.T, events []handlerEvent) {
				assert.Equal(
					t,
					[]handlerEvent{
						{event: handlerEventAdd, newObj: slice1},
					},
					events,
				)
			},
		},
		"runtime-CEL-errors-skip-devices": {
			inputEvents: func(event inputEventGenerator) {
				event.addDeviceTaintRule(taintNoDevicesCELRuntimeErrorRule)
				event.addResourceSlice(slice1)
			},
			expectedPatchedSlices: []*resourceapi.ResourceSlice{
				slice1,
			},
			expectEvents: func(t *assert.CollectT, events *v1.EventList) {
				if !assert.Len(t, events.Items, 1) {
					return
				}
				assert.Equal(t, taintNoDevicesCELRuntimeErrorRule.Name, events.Items[0].InvolvedObject.Name)
				assert.Equal(t, "CELRuntimeError", events.Items[0].Reason)
			},
			expectHandlerEvents: func(t *testing.T, events []handlerEvent) {
				assert.Equal(
					t,
					[]handlerEvent{
						{event: handlerEventAdd, newObj: slice1},
					},
					events,
				)
			},
		},
		"invalid-CEL-expression-throws-error": {
			inputEvents: func(event inputEventGenerator) {
				event.addDeviceTaintRule(taintNoDevicesInvalidCELRule)
				event.addResourceSlice(slice1)
			},
			expectedPatchedSlices: []*resourceapi.ResourceSlice{},
			expectUnhandledErrors: func(t *testing.T, errs []error) {
				if !assert.Len(t, errs, 1) {
					return
				}
				assert.ErrorContains(t, errs[0], "CEL compile error")
			},
		},
		"add-taint-for-device-class": {
			inputEvents: func(event inputEventGenerator) {
				event.addDeviceClass(deviceClass1)
				event.addDeviceTaintRule(taintDeviceClass1Rule)
				event.addResourceSlice(slice1)
				event.addResourceSlice(slice2)
			},
			expectedPatchedSlices: []*resourceapi.ResourceSlice{
				slice1Tainted,
				slice2,
			},
			expectHandlerEvents: func(t *testing.T, events []handlerEvent) {
				assert.Equal(
					t,
					[]handlerEvent{
						{event: handlerEventAdd, newObj: slice1Tainted},
						{event: handlerEventAdd, newObj: slice2},
					},
					events,
				)
			},
		},
		"filter-all-criteria": {
			inputEvents: func(event inputEventGenerator) {
				event.addDeviceClass(deviceClass1)
				event.addDeviceTaintRule(
					taintDeviceClassRule(
						taintDriverDevicesRule(
							taintPoolDevicesRule(
								taintNamedDevicesRule(
									taintCELSelectedDevicesRule(
										taintAllDevicesRule,
										`true`,
									),
									device1Name,
								),
								pool1,
							),
							driver1,
						),
						deviceClass1.Name,
					),
				)
				event.addResourceSlice(slice1)
				event.addResourceSlice(slice2)
			},
			expectedPatchedSlices: []*resourceapi.ResourceSlice{
				slice1Tainted,
				slice2,
			},
			expectHandlerEvents: func(t *testing.T, events []handlerEvent) {
				assert.Equal(
					t,
					[]handlerEvent{
						{event: handlerEventAdd, newObj: slice1Tainted},
						{event: handlerEventAdd, newObj: slice2},
					},
					events,
				)
			},
		},
		"update-patched-slice": {
			inputEvents: func(event inputEventGenerator) {
				event.addDeviceTaintRule(taintDevice1Rule)
				event.addResourceSlice(slice1)
				event.addResourceSlice(sliceWithDevices(slice1, threeDevices))
				event.addResourceSlice(sliceWithDevices(slice2, threeDevices))
				event.addResourceSlice(sliceWithDevices(slice2, devices))
			},
			expectedPatchedSlices: []*resourceapi.ResourceSlice{
				sliceWithDevices(slice1, threeDevicesOneTainted),
				sliceWithDevices(slice2, taintedDevices),
			},
			expectHandlerEvents: func(t *testing.T, events []handlerEvent) {
				assert.Equal(
					t,
					[]handlerEvent{
						{event: handlerEventAdd, newObj: slice1Tainted},
						{event: handlerEventUpdate, oldObj: slice1Tainted, newObj: sliceWithDevices(slice1, threeDevicesOneTainted)},
						{event: handlerEventAdd, newObj: sliceWithDevices(slice2, threeDevicesOneTainted)},
						{event: handlerEventUpdate, oldObj: sliceWithDevices(slice2, threeDevicesOneTainted), newObj: sliceWithDevices(slice2, taintedDevices)},
					},
					events,
				)
			},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)

			kubeClient := fake.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactoryWithOptions(kubeClient, 10*time.Minute)

			var handlerEvents []handlerEvent
			handler := cache.ResourceEventHandlerFuncs{
				AddFunc: func(obj interface{}) {
					handlerEvents = append(handlerEvents, handlerEvent{event: handlerEventAdd, newObj: obj.(*resourceapi.ResourceSlice)})
				},
				UpdateFunc: func(oldObj, newObj interface{}) {
					handlerEvents = append(handlerEvents, handlerEvent{event: handlerEventUpdate, oldObj: oldObj.(*resourceapi.ResourceSlice), newObj: newObj.(*resourceapi.ResourceSlice)})
				},
				DeleteFunc: func(obj interface{}) {
					handlerEvents = append(handlerEvents, handlerEvent{event: handlerEventDelete, oldObj: obj.(*resourceapi.ResourceSlice)})
				},
			}

			opts := Options{
				EnableDeviceTaints: !test.deviceTaintsDisabled,
				SliceInformer:      informerFactory.Resource().V1beta1().ResourceSlices(),
				TaintInformer:      informerFactory.Resource().V1alpha3().DeviceTaintRules(),
				ClassInformer:      informerFactory.Resource().V1beta1().DeviceClasses(),
				KubeClient:         kubeClient,
			}
			tracker, err := newTracker(ctx, opts)
			require.NoError(t, err)
			var unhandledErrors []error
			tracker.handleError = func(_ context.Context, err error, _ string, _ ...any) {
				unhandledErrors = append(unhandledErrors, err)
			}
			_, _ = tracker.AddEventHandler(handler)

			if test.inputEvents != nil {
				test.inputEvents(inputEventGeneratorForTest(ctx, t, tracker))
			}

			expectHandlerEvents := test.expectHandlerEvents
			if expectHandlerEvents == nil {
				expectHandlerEvents = func(t *testing.T, events []handlerEvent) {
					assert.Empty(t, events)
				}
			}
			expectHandlerEvents(t, handlerEvents)

			expectUnhandledErrors := test.expectUnhandledErrors
			if expectUnhandledErrors == nil {
				expectUnhandledErrors = func(t *testing.T, errs []error) {
					assert.Empty(t, errs)
				}
			}
			expectUnhandledErrors(t, unhandledErrors)

			// Check ResourceSlices
			patchedResourceSlices, err := tracker.ListPatchedResourceSlices()
			require.NoError(t, err, "list patched resource slices")
			sortResourceSlicesFunc := func(s1, s2 *resourceapi.ResourceSlice) int {
				return stdcmp.Compare(s1.Name, s2.Name)
			}
			slices.SortFunc(test.expectedPatchedSlices, sortResourceSlicesFunc)
			slices.SortFunc(patchedResourceSlices, sortResourceSlicesFunc)
			assert.Equal(t, test.expectedPatchedSlices, patchedResourceSlices)
			expectEvents := test.expectEvents
			if expectEvents == nil {
				expectEvents = func(t *assert.CollectT, events *v1.EventList) {
					assert.Empty(t, events.Items)
				}
			}
			// Events are generated asynchronously. While shutting down the event recorder will flush all
			// pending events, it is not possible to determine when exactly that flush is complete.
			assert.EventuallyWithT(
				t,
				func(t *assert.CollectT) {
					events, err := kubeClient.CoreV1().Events("").List(ctx, metav1.ListOptions{})
					require.NoError(t, err, "list events")
					expectEvents(t, events)
				},
				1*time.Second,
				10*time.Millisecond,
				"did not observe expected events",
			)
		})
	}
}

func BenchmarkEventHandlers(b *testing.B) {
	now := time.Now()
	benchmarks := map[string]struct {
		resourceSlices []*resourceapi.ResourceSlice
		taintRules     []*resourcealphaapi.DeviceTaintRule
		loop           func(ctx context.Context, b *testing.B, tracker *Tracker, resourceSlices []*resourceapi.ResourceSlice, taintRules []*resourcealphaapi.DeviceTaintRule, i int)
	}{
		"resource-slice-add-no-taint-rules": {
			resourceSlices: func() []*resourceapi.ResourceSlice {
				resourceSlices := make([]*resourceapi.ResourceSlice, 1000)
				for i := range resourceSlices {
					resourceSlices[i] = &resourceapi.ResourceSlice{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slice-" + strconv.Itoa(i),
						},
						Spec: resourceapi.ResourceSliceSpec{
							Devices: slices.Repeat([]resourceapi.Device{{Basic: &resourceapi.BasicDevice{}}}, 64),
						},
					}
				}
				return resourceSlices
			}(),
			loop: func(ctx context.Context, b *testing.B, tracker *Tracker, resourceSlices []*resourceapi.ResourceSlice, _ []*resourcealphaapi.DeviceTaintRule, i int) {
				tracker.resourceSliceAdd(ctx)(resourceSlices[i%len(resourceSlices)])
			},
		},
		"one-patch-to-many-slices-add-taint-rule": {
			resourceSlices: func() []*resourceapi.ResourceSlice {
				resourceSlices := make([]*resourceapi.ResourceSlice, 500)
				for i := range resourceSlices {
					resourceSlices[i] = &resourceapi.ResourceSlice{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slice-" + strconv.Itoa(i),
						},
						Spec: resourceapi.ResourceSliceSpec{
							Devices: slices.Repeat([]resourceapi.Device{{Basic: &resourceapi.BasicDevice{}}}, 64),
						},
					}
				}
				return resourceSlices
			}(),
			taintRules: []*resourcealphaapi.DeviceTaintRule{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "taintRule",
					},
					Spec: resourcealphaapi.DeviceTaintRuleSpec{
						DeviceSelector: nil, // all slices
						Taint: resourcealphaapi.DeviceTaint{
							Key:       "example.com/taint",
							Value:     "tainted",
							Effect:    resourcealphaapi.DeviceTaintEffectNoExecute,
							TimeAdded: &metav1.Time{Time: now},
						},
					},
				},
			},
			loop: func(ctx context.Context, b *testing.B, tracker *Tracker, resourceSlices []*resourceapi.ResourceSlice, taintRules []*resourcealphaapi.DeviceTaintRule, i int) {
				tracker.deviceTaintAdd(ctx)(taintRules[i%len(taintRules)])
			},
		},
		"one-patch-to-many-slices-add-slice": {
			resourceSlices: func() []*resourceapi.ResourceSlice {
				resourceSlices := make([]*resourceapi.ResourceSlice, 500)
				for i := range resourceSlices {
					resourceSlices[i] = &resourceapi.ResourceSlice{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slice-" + strconv.Itoa(i),
						},
						Spec: resourceapi.ResourceSliceSpec{
							Devices: slices.Repeat([]resourceapi.Device{{Basic: &resourceapi.BasicDevice{}}}, 64),
						},
					}
				}
				return resourceSlices
			}(),
			taintRules: []*resourcealphaapi.DeviceTaintRule{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "taintRule",
					},
					Spec: resourcealphaapi.DeviceTaintRuleSpec{
						DeviceSelector: nil, // all slices
						Taint: resourcealphaapi.DeviceTaint{
							Key:       "example.com/taint",
							Value:     "tainted",
							Effect:    resourcealphaapi.DeviceTaintEffectNoExecute,
							TimeAdded: &metav1.Time{Time: now},
						},
					},
				},
			},
			loop: func(ctx context.Context, b *testing.B, tracker *Tracker, resourceSlices []*resourceapi.ResourceSlice, _ []*resourcealphaapi.DeviceTaintRule, i int) {
				tracker.resourceSliceAdd(ctx)(resourceSlices[i%len(resourceSlices)])
			},
		},
		"one-patched-device-among-many-slices-add-taint-rule": {
			resourceSlices: func() []*resourceapi.ResourceSlice {
				nSlices := 500
				nDevices := 64
				resourceSlices := make([]*resourceapi.ResourceSlice, nSlices)
				for i := range resourceSlices {
					resourceSlices[i] = &resourceapi.ResourceSlice{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slice-" + strconv.Itoa(i),
						},
						Spec: resourceapi.ResourceSliceSpec{
							Pool: resourceapi.ResourcePool{
								Name: "pool-" + strconv.Itoa(i),
							},
							Devices: func() []resourceapi.Device {
								devices := make([]resourceapi.Device, nDevices)
								for j := range devices {
									devices[j] = resourceapi.Device{
										Name:  "device-" + strconv.Itoa(j),
										Basic: &resourceapi.BasicDevice{},
									}
								}
								return devices
							}(),
						},
					}
				}
				resourceSlices[nSlices/2].Spec.Devices[nDevices/2].Name = "patchme"
				return resourceSlices
			}(),
			taintRules: []*resourcealphaapi.DeviceTaintRule{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "taintRule",
					},
					Spec: resourcealphaapi.DeviceTaintRuleSpec{
						DeviceSelector: &resourcealphaapi.DeviceTaintSelector{
							Device: ptr.To("patchme"),
						},
						Taint: resourcealphaapi.DeviceTaint{
							Key:       "example.com/taint",
							Value:     "tainted",
							Effect:    resourcealphaapi.DeviceTaintEffectNoExecute,
							TimeAdded: &metav1.Time{Time: now},
						},
					},
				},
			},
			loop: func(ctx context.Context, b *testing.B, tracker *Tracker, resourceSlices []*resourceapi.ResourceSlice, taintRules []*resourcealphaapi.DeviceTaintRule, i int) {
				tracker.deviceTaintAdd(ctx)(taintRules[i%len(taintRules)])
			},
		},
		"one-patched-device-among-many-slices-add-slice": {
			resourceSlices: func() []*resourceapi.ResourceSlice {
				resourceSlices := make([]*resourceapi.ResourceSlice, 500)
				for i := range resourceSlices {
					resourceSlices[i] = &resourceapi.ResourceSlice{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slice-" + strconv.Itoa(i),
						},
						Spec: resourceapi.ResourceSliceSpec{
							Pool: resourceapi.ResourcePool{
								Name: "pool-" + strconv.Itoa(i),
							},
							Devices: func() []resourceapi.Device {
								nDevices := 64
								devices := slices.Repeat([]resourceapi.Device{{Basic: &resourceapi.BasicDevice{}}}, nDevices)
								devices[nDevices/2].Name = "patchme"
								return devices
							}(),
						},
					}
				}
				return resourceSlices
			}(),
			taintRules: []*resourcealphaapi.DeviceTaintRule{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "patch",
					},
					Spec: resourcealphaapi.DeviceTaintRuleSpec{
						DeviceSelector: &resourcealphaapi.DeviceTaintSelector{
							Pool:   ptr.To("pool-250"),
							Device: ptr.To("patchme"),
						},
						Taint: resourcealphaapi.DeviceTaint{
							Key:       "example.com/taint",
							Value:     "tainted",
							Effect:    resourcealphaapi.DeviceTaintEffectNoExecute,
							TimeAdded: &metav1.Time{Time: now},
						},
					},
				},
			},
			loop: func(ctx context.Context, b *testing.B, tracker *Tracker, resourceSlices []*resourceapi.ResourceSlice, patches []*resourcealphaapi.DeviceTaintRule, i int) {
				tracker.resourceSliceAdd(ctx)(resourceSlices[250]) // the slice affected by the patch
			},
		},
		"one-patch-for-each-of-many-slices-add-taint-rule": {
			resourceSlices: func() []*resourceapi.ResourceSlice {
				resourceSlices := make([]*resourceapi.ResourceSlice, 500)
				for i := range resourceSlices {
					resourceSlices[i] = &resourceapi.ResourceSlice{
						ObjectMeta: metav1.ObjectMeta{
							Name: "slice-" + strconv.Itoa(i),
						},
						Spec: resourceapi.ResourceSliceSpec{
							Pool: resourceapi.ResourcePool{
								Name: "pool-" + strconv.Itoa(i),
							},
							Devices: slices.Repeat([]resourceapi.Device{{Basic: &resourceapi.BasicDevice{}}}, 64),
						},
					}
				}
				return resourceSlices
			}(),
			taintRules: func() []*resourcealphaapi.DeviceTaintRule {
				patches := make([]*resourcealphaapi.DeviceTaintRule, 500)
				for i := range patches {
					patches[i] = &resourcealphaapi.DeviceTaintRule{
						ObjectMeta: metav1.ObjectMeta{
							Name: "taint-rule-" + strconv.Itoa(i),
						},
						Spec: resourcealphaapi.DeviceTaintRuleSpec{
							DeviceSelector: &resourcealphaapi.DeviceTaintSelector{
								Pool: ptr.To("pool-" + strconv.Itoa(i)),
							},
							Taint: resourcealphaapi.DeviceTaint{
								Key:       "example.com/taint",
								Value:     "tainted",
								Effect:    resourcealphaapi.DeviceTaintEffectNoExecute,
								TimeAdded: &metav1.Time{Time: now},
							},
						},
					}
				}
				return patches
			}(),
			loop: func(ctx context.Context, b *testing.B, tracker *Tracker, resourceSlices []*resourceapi.ResourceSlice, taintRules []*resourcealphaapi.DeviceTaintRule, i int) {
				tracker.deviceTaintAdd(ctx)(taintRules[i%len(taintRules)])
			},
		},
	}

	newBenchTracker := func(ctx context.Context) *Tracker {
		kubeClient := fake.NewSimpleClientset()
		informerFactory := informers.NewSharedInformerFactoryWithOptions(kubeClient, 10*time.Minute)
		opts := Options{
			EnableDeviceTaints: true,
			SliceInformer:      informerFactory.Resource().V1beta1().ResourceSlices(),
			TaintInformer:      informerFactory.Resource().V1alpha3().DeviceTaintRules(),
			ClassInformer:      informerFactory.Resource().V1beta1().DeviceClasses(),
			KubeClient:         kubeClient,
		}
		tracker, err := newTracker(ctx, opts)
		require.NoError(b, err)
		tracker.handleError = func(_ context.Context, err error, _ string, _ ...any) {
			b.Error("unexpected unhandled error:", err)
		}
		return tracker
	}

	for name, benchmark := range benchmarks {
		b.Run(name, func(b *testing.B) {
			logger, ctx := ktesting.NewTestContext(b)
			ctx = klog.NewContext(ctx, logger.V(2))
			tracker := newBenchTracker(ctx)

			for _, slice := range benchmark.resourceSlices {
				err := tracker.resourceSlices.GetIndexer().Add(slice)
				require.NoError(b, err)
			}

			for _, taintRule := range benchmark.taintRules {
				err := tracker.deviceTaints.GetIndexer().Add(taintRule)
				require.NoError(b, err)
			}

			b.ResetTimer()
			for i := range b.N {
				benchmark.loop(ctx, b, tracker, benchmark.resourceSlices, benchmark.taintRules, i)
			}
		})
	}
}
