/*
 * Copyright 2020 The Knative Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package broker_test // different package name due to import cycles. (broker -> testing -> broker)

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/Shopify/sarama"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgotesting "k8s.io/client-go/testing"
	eventing "knative.dev/eventing/pkg/apis/eventing/v1beta1"
	"knative.dev/pkg/apis"
	kubeclient "knative.dev/pkg/client/injection/kube/client/fake"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/logging"
	. "knative.dev/pkg/reconciler/testing"
	"knative.dev/pkg/resolver"

	eventingduck "knative.dev/eventing/pkg/apis/duck/v1beta1"
	fakeeventingclient "knative.dev/eventing/pkg/client/injection/client/fake"
	brokerreconciler "knative.dev/eventing/pkg/client/injection/reconciler/eventing/v1beta1/broker"
	reconcilertesting "knative.dev/eventing/pkg/reconciler/testing/v1beta1"
	duckv1 "knative.dev/pkg/apis/duck/v1"

	coreconfig "knative.dev/eventing-kafka-broker/control-plane/pkg/core/config"
	"knative.dev/eventing-kafka-broker/control-plane/pkg/reconciler/base"
	. "knative.dev/eventing-kafka-broker/control-plane/pkg/reconciler/broker"
	"knative.dev/eventing-kafka-broker/control-plane/pkg/reconciler/kafka"
	. "knative.dev/eventing-kafka-broker/control-plane/pkg/reconciler/testing"
)

const (
	wantErrorOnCreateTopic = "wantErrorOnCreateTopic"
	wantErrorOnDeleteTopic = "wantErrorOnDeleteTopic"
)

const (
	finalizerName = "brokers.eventing.knative.dev"
)

var (
	finalizerUpdatedEvent = Eventf(
		corev1.EventTypeNormal,
		"FinalizerUpdate",
		fmt.Sprintf(`Updated %q finalizers`, BrokerName),
	)

	createTopicError = fmt.Errorf("failed to create topic")
	deleteTopicError = fmt.Errorf("failed to delete topic")
)

func TestBrokeReconciler(t *testing.T) {
	eventing.RegisterAlternateBrokerConditionSet(ConditionSet)

	t.Parallel()

	for _, f := range Formats {
		brokerReconciliation(t, f, *DefaultConfigs)
	}
}

func brokerReconciliation(t *testing.T, format string, configs Configs) {

	testKey := fmt.Sprintf("%s/%s", BrokerNamespace, BrokerName)

	configs.DataPlaneConfigFormat = format

	table := TableTest{
		{
			Name: "Reconciled normal - no DLS",
			Objects: []runtime.Object{
				NewBroker(),
				NewConfigMap(&configs, nil),
				NewService(),
				NewReceiverPod(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "1",
					"annotation_to_preserve":           "value_to_preserve",
				}),
				NewDispatcherPod(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "2",
					"annotation_to_preserve":           "value_to_preserve",
				}),
			},
			Key: testKey,
			WantEvents: []string{
				finalizerUpdatedEvent,
				Eventf(
					corev1.EventTypeNormal,
					Reconciled,
					fmt.Sprintf(`%s reconciled: "%s/%s"`, Broker, BrokerNamespace, BrokerName),
				),
			},
			WantUpdates: []clientgotesting.UpdateActionImpl{
				ConfigMapUpdate(&configs, &coreconfig.Brokers{
					Broker: []*coreconfig.Broker{
						{
							Id:        BrokerUUID,
							Topic:     GetTopic(),
							Namespace: BrokerNamespace,
							Name:      BrokerName,
						},
					},
					VolumeGeneration: 1,
				}),
				ReceiverPodUpdate(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "1",
					"annotation_to_preserve":           "value_to_preserve",
				}),
				DispatcherPodUpdate(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "1",
					"annotation_to_preserve":           "value_to_preserve",
				}),
			},
			WantPatches: []clientgotesting.PatchActionImpl{
				patchFinalizers(),
			},
			WantStatusUpdates: []clientgotesting.UpdateActionImpl{
				{
					Object: NewBroker(
						reconcilertesting.WithInitBrokerConditions,
						ConfigMapUpdatedReady(&configs),
						TopicReady,
						Addressable(&configs),
					),
				},
			},
		},
		{
			Name: "Reconciled normal - with DLS",
			Objects: []runtime.Object{
				NewBroker(
					WithDelivery(),
				),
				NewConfigMapFromBrokers(&coreconfig.Brokers{
					VolumeGeneration: 1,
				}, &configs),
				NewService(),
				NewReceiverPod(configs.SystemNamespace, map[string]string{base.VolumeGenerationAnnotationKey: "2"}),
				NewDispatcherPod(configs.SystemNamespace, map[string]string{base.VolumeGenerationAnnotationKey: "2"}),
			},
			Key: testKey,
			WantEvents: []string{
				finalizerUpdatedEvent,
				Eventf(
					corev1.EventTypeNormal,
					Reconciled,
					fmt.Sprintf(`%s reconciled: "%s/%s"`, Broker, BrokerNamespace, BrokerName),
				),
			},
			WantUpdates: []clientgotesting.UpdateActionImpl{
				ConfigMapUpdate(&configs, &coreconfig.Brokers{
					Broker: []*coreconfig.Broker{
						{
							Id:             BrokerUUID,
							Topic:          GetTopic(),
							DeadLetterSink: "http://test-service.test-service-namespace.svc.cluster.local/",
							Namespace:      BrokerNamespace,
							Name:           BrokerName,
						},
					},
					VolumeGeneration: 2,
				}),
				ReceiverPodUpdate(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "2",
				}),
				DispatcherPodUpdate(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "2",
				}),
			},
			WantPatches: []clientgotesting.PatchActionImpl{
				patchFinalizers(),
			},
			WantStatusUpdates: []clientgotesting.UpdateActionImpl{
				{
					Object: NewBroker(
						WithDelivery(),
						reconcilertesting.WithInitBrokerConditions,
						ConfigMapUpdatedReady(&configs),
						TopicReady,
						Addressable(&configs),
					),
				},
			},
		},
		{
			Name: "Failed to create topic",
			Objects: []runtime.Object{
				NewBroker(),
			},
			Key:     testKey,
			WantErr: true,
			WantEvents: []string{
				finalizerUpdatedEvent,
				Eventf(
					corev1.EventTypeWarning,
					"InternalError",
					"failed to create topic: %s: %v",
					GetTopic(), createTopicError,
				),
			},
			WantPatches: []clientgotesting.PatchActionImpl{
				patchFinalizers(),
			},
			WantStatusUpdates: []clientgotesting.UpdateActionImpl{
				{
					Object: NewBroker(
						reconcilertesting.WithInitBrokerConditions,
						FailedToCreateTopic,
					),
				},
			},
			OtherTestData: map[string]interface{}{
				wantErrorOnCreateTopic: createTopicError,
			},
		},
		{
			Name: "Failed to get config map",
			Objects: []runtime.Object{
				NewBroker(),
				&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: configs.DataPlaneConfigMapNamespace,
						Name:      configs.DataPlaneConfigMapName + "a",
					},
				},
			},
			Key:     testKey,
			WantErr: true,
			WantEvents: []string{
				finalizerUpdatedEvent,
				Eventf(
					corev1.EventTypeWarning,
					"InternalError",
					"failed to get brokers and triggers config map %s: %v",
					configs.DataPlaneConfigMapAsString(), `configmaps "knative-eventing" not found`,
				),
			},
			WantPatches: []clientgotesting.PatchActionImpl{
				patchFinalizers(),
			},
			WantStatusUpdates: []clientgotesting.UpdateActionImpl{
				{
					Object: NewBroker(
						reconcilertesting.WithInitBrokerConditions,
						TopicReady,
						FailedToGetConfigMap(&configs),
					),
				},
			},
		},
		{
			Name: "Reconciled normal - config map not readable",
			Objects: []runtime.Object{
				NewBroker(),
				NewConfigMap(&configs, []byte(`{"hello": "world"}`)),
				NewService(),
				NewReceiverPod(configs.SystemNamespace, nil),
				NewDispatcherPod(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "2",
				}),
			},
			Key: testKey,
			WantEvents: []string{
				finalizerUpdatedEvent,
				Eventf(
					corev1.EventTypeNormal,
					Reconciled,
					fmt.Sprintf(`%s reconciled: "%s/%s"`, Broker, BrokerNamespace, BrokerName),
				),
			},
			WantUpdates: []clientgotesting.UpdateActionImpl{
				ConfigMapUpdate(&configs, &coreconfig.Brokers{
					Broker: []*coreconfig.Broker{
						{
							Id:        BrokerUUID,
							Topic:     GetTopic(),
							Namespace: BrokerNamespace,
							Name:      BrokerName,
						},
					},
					VolumeGeneration: 1,
				}),
				ReceiverPodUpdate(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "1",
				}),
				DispatcherPodUpdate(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "1",
				}),
			},
			WantPatches: []clientgotesting.PatchActionImpl{
				patchFinalizers(),
			},
			WantStatusUpdates: []clientgotesting.UpdateActionImpl{
				{
					Object: NewBroker(
						reconcilertesting.WithInitBrokerConditions,
						ConfigMapUpdatedReady(&configs),
						TopicReady,
						Addressable(&configs),
					),
				},
			},
		},
		{
			Name: "Reconciled normal - preserve config map previous state",
			Objects: []runtime.Object{
				NewBroker(),
				NewConfigMapFromBrokers(&coreconfig.Brokers{
					Broker: []*coreconfig.Broker{
						{
							Id:             "5384faa4-6bdf-428d-b6c2-d6f89ce1d44b",
							Topic:          "my-existing-topic-a",
							DeadLetterSink: "http://www.my-sink.com",
							Namespace:      BrokerNamespace,
							Name:           BrokerName,
						},
						{
							Id:             "5384faa4-6bdf-428d-b6c2-d6f89ce1d44a",
							Topic:          "my-existing-topic-b",
							DeadLetterSink: "http://www.my-sink.com",
						},
					},
				}, &configs),
				NewService(),
				NewReceiverPod(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "2",
				}),
				NewDispatcherPod(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "2",
				}),
			},
			Key: testKey,
			WantEvents: []string{
				finalizerUpdatedEvent,
				Eventf(
					corev1.EventTypeNormal,
					Reconciled,
					fmt.Sprintf(`%s reconciled: "%s/%s"`, Broker, BrokerNamespace, BrokerName),
				),
			},
			WantUpdates: []clientgotesting.UpdateActionImpl{
				ConfigMapUpdate(&configs, &coreconfig.Brokers{
					Broker: []*coreconfig.Broker{
						{
							Id:             "5384faa4-6bdf-428d-b6c2-d6f89ce1d44b",
							Topic:          "my-existing-topic-a",
							DeadLetterSink: "http://www.my-sink.com",
							Namespace:      BrokerNamespace,
							Name:           BrokerName,
						},
						{
							Id:             "5384faa4-6bdf-428d-b6c2-d6f89ce1d44a",
							Topic:          "my-existing-topic-b",
							DeadLetterSink: "http://www.my-sink.com",
						},
						{
							Id:        BrokerUUID,
							Topic:     GetTopic(),
							Namespace: BrokerNamespace,
							Name:      BrokerName,
						},
					},
					VolumeGeneration: 1,
				}),
				ReceiverPodUpdate(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "1",
				}),
				DispatcherPodUpdate(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "1",
				}),
			},
			WantPatches: []clientgotesting.PatchActionImpl{
				patchFinalizers(),
			},
			WantStatusUpdates: []clientgotesting.UpdateActionImpl{
				{
					Object: NewBroker(
						reconcilertesting.WithInitBrokerConditions,
						ConfigMapUpdatedReady(&configs),
						TopicReady,
						Addressable(&configs),
					),
				},
			},
		},
		{
			Name: "Reconciled normal - update existing broker while preserving others",
			Objects: []runtime.Object{
				NewBroker(
					func(broker *eventing.Broker) {
						broker.Spec.Delivery = &eventingduck.DeliverySpec{
							DeadLetterSink: &duckv1.Destination{
								URI: &apis.URL{
									Scheme: "http",
									Host:   "www.my-sink.com",
									Path:   "/api",
								},
							},
						}
					},
				),
				NewConfigMapFromBrokers(&coreconfig.Brokers{
					Broker: []*coreconfig.Broker{
						{
							Id:             "5384faa4-6bdf-428d-b6c2-d6f89ce1d44b",
							Topic:          "my-existing-topic-a",
							DeadLetterSink: "http://www.my-sink.com",
						},
						{
							Id:             BrokerUUID,
							Topic:          GetTopic(),
							DeadLetterSink: "http://www.my-sink.com",
						},
					},
				}, &configs),
				NewService(),
				NewReceiverPod(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "5",
				}),
				NewDispatcherPod(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "5",
				}),
			},
			Key: testKey,
			WantEvents: []string{
				finalizerUpdatedEvent,
				Eventf(
					corev1.EventTypeNormal,
					Reconciled,
					fmt.Sprintf(`%s reconciled: "%s/%s"`, Broker, BrokerNamespace, BrokerName),
				),
			},
			WantUpdates: []clientgotesting.UpdateActionImpl{
				ConfigMapUpdate(&configs, &coreconfig.Brokers{
					Broker: []*coreconfig.Broker{
						{
							Id:             "5384faa4-6bdf-428d-b6c2-d6f89ce1d44b",
							Topic:          "my-existing-topic-a",
							DeadLetterSink: "http://www.my-sink.com",
						},
						{
							Id:             BrokerUUID,
							Topic:          GetTopic(),
							DeadLetterSink: "http://www.my-sink.com/api",
							Namespace:      BrokerNamespace,
							Name:           BrokerName,
						},
					},
					VolumeGeneration: 1,
				}),
				ReceiverPodUpdate(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "1",
				}),
				DispatcherPodUpdate(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "1",
				}),
			},
			WantPatches: []clientgotesting.PatchActionImpl{
				patchFinalizers(),
			},
			WantStatusUpdates: []clientgotesting.UpdateActionImpl{
				{
					Object: NewBroker(
						func(broker *eventing.Broker) {
							broker.Spec.Delivery = &eventingduck.DeliverySpec{
								DeadLetterSink: &duckv1.Destination{
									URI: func() *apis.URL {
										URL, _ := url.Parse("http://www.my-sink.com/api")
										return (*apis.URL)(URL)
									}(),
								},
							}
						},
						reconcilertesting.WithInitBrokerConditions,
						ConfigMapUpdatedReady(&configs),
						TopicReady,
						Addressable(&configs),
					),
				},
			},
		},
		{
			Name: "Reconciled normal - remove existing broker DLS while preserving others",
			Objects: []runtime.Object{
				NewBroker(
					func(broker *eventing.Broker) {
						broker.Spec.Delivery = &eventingduck.DeliverySpec{}
					},
				),
				NewConfigMapFromBrokers(&coreconfig.Brokers{
					Broker: []*coreconfig.Broker{
						{
							Id:             "5384faa4-6bdf-428d-b6c2-d6f89ce1d44b",
							Topic:          "my-existing-topic-a",
							DeadLetterSink: "http://www.my-sink.com",
							Namespace:      BrokerNamespace,
							Name:           BrokerName,
						},
						{
							Id:             BrokerUUID,
							Topic:          GetTopic(),
							DeadLetterSink: "http://www.my-sink.com",
							Namespace:      BrokerNamespace,
							Name:           BrokerName,
						},
					},
				}, &configs),
				NewService(),
				NewReceiverPod(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "5",
				}),
				NewDispatcherPod(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "5",
				}),
			},
			Key: testKey,
			WantEvents: []string{
				finalizerUpdatedEvent,
				Eventf(
					corev1.EventTypeNormal,
					Reconciled,
					fmt.Sprintf(`%s reconciled: "%s/%s"`, Broker, BrokerNamespace, BrokerName),
				),
			},
			WantUpdates: []clientgotesting.UpdateActionImpl{
				ConfigMapUpdate(&configs, &coreconfig.Brokers{
					Broker: []*coreconfig.Broker{
						{
							Id:             "5384faa4-6bdf-428d-b6c2-d6f89ce1d44b",
							Topic:          "my-existing-topic-a",
							DeadLetterSink: "http://www.my-sink.com",
							Namespace:      BrokerNamespace,
							Name:           BrokerName,
						},
						{
							Id:        BrokerUUID,
							Topic:     GetTopic(),
							Namespace: BrokerNamespace,
							Name:      BrokerName,
						},
					},
					VolumeGeneration: 1,
				}),
				ReceiverPodUpdate(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "1",
				}),
				DispatcherPodUpdate(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "1",
				}),
			},
			WantPatches: []clientgotesting.PatchActionImpl{
				patchFinalizers(),
			},
			WantStatusUpdates: []clientgotesting.UpdateActionImpl{
				{
					Object: NewBroker(
						func(broker *eventing.Broker) {
							broker.Spec.Delivery = &eventingduck.DeliverySpec{}
						},
						reconcilertesting.WithInitBrokerConditions,
						ConfigMapUpdatedReady(&configs),
						TopicReady,
						Addressable(&configs),
					),
				},
			},
		},
		{
			Name: "Reconciled normal - increment volume generation",
			Objects: []runtime.Object{
				NewBroker(),
				NewConfigMapFromBrokers(&coreconfig.Brokers{
					Broker: []*coreconfig.Broker{
						{
							Id:             "5384faa4-6bdf-428d-b6c2-d6f89ce1d44b",
							Topic:          "my-existing-topic-a",
							DeadLetterSink: "http://www.my-sink.com",
							Namespace:      BrokerNamespace,
							Name:           BrokerName,
						},
						{
							Id:        BrokerUUID,
							Topic:     GetTopic(),
							Namespace: BrokerNamespace,
							Name:      BrokerName,
						},
					},
					VolumeGeneration: 1,
				}, &configs),
				NewService(),
				NewReceiverPod(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "5",
				}),
				NewDispatcherPod(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "5",
				}),
			},
			Key: testKey,
			WantEvents: []string{
				finalizerUpdatedEvent,
				Eventf(
					corev1.EventTypeNormal,
					Reconciled,
					fmt.Sprintf(`%s reconciled: "%s/%s"`, Broker, BrokerNamespace, BrokerName),
				),
			},
			WantUpdates: []clientgotesting.UpdateActionImpl{
				ConfigMapUpdate(&configs, &coreconfig.Brokers{
					Broker: []*coreconfig.Broker{
						{
							Id:             "5384faa4-6bdf-428d-b6c2-d6f89ce1d44b",
							Topic:          "my-existing-topic-a",
							DeadLetterSink: "http://www.my-sink.com",
							Namespace:      BrokerNamespace,
							Name:           BrokerName,
						},
						{
							Id:        BrokerUUID,
							Topic:     GetTopic(),
							Namespace: BrokerNamespace,
							Name:      BrokerName,
						},
					},
					VolumeGeneration: 2,
				}),
				ReceiverPodUpdate(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "2",
				}),
				DispatcherPodUpdate(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "2",
				}),
			},
			WantPatches: []clientgotesting.PatchActionImpl{
				patchFinalizers(),
			},
			WantStatusUpdates: []clientgotesting.UpdateActionImpl{
				{
					Object: NewBroker(
						reconcilertesting.WithInitBrokerConditions,
						ConfigMapUpdatedReady(&configs),
						TopicReady,
						Addressable(&configs),
					),
				},
			},
		},
		{
			Name: "Failed to resolve DLS",
			Objects: []runtime.Object{
				NewBroker(
					func(broker *eventing.Broker) {
						broker.Spec.Delivery = &eventingduck.DeliverySpec{
							DeadLetterSink: &duckv1.Destination{},
						}
					},
				),
				NewConfigMapFromBrokers(&coreconfig.Brokers{
					Broker: []*coreconfig.Broker{
						{
							Id:             "5384faa4-6bdf-428d-b6c2-d6f89ce1d44b",
							Topic:          "my-existing-topic-a",
							DeadLetterSink: "http://www.my-sink.com",
							Namespace:      BrokerNamespace,
							Name:           BrokerName,
						},
						{
							Id:        BrokerUUID,
							Topic:     GetTopic(),
							Namespace: BrokerNamespace,
							Name:      BrokerName,
						},
					},
					VolumeGeneration: 1,
				}, &configs),
				NewReceiverPod(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "5",
				}),
				NewDispatcherPod(configs.SystemNamespace, map[string]string{
					base.VolumeGenerationAnnotationKey: "5",
				}),
			},
			Key:     testKey,
			WantErr: true,
			WantEvents: []string{
				finalizerUpdatedEvent,
				Eventf(
					corev1.EventTypeWarning,
					"InternalError",
					"failed to get broker configuration: failed to resolve broker.Spec.Deliver.DeadLetterSink: %v",
					"destination missing Ref and URI, expected at least one",
				),
			},
			WantPatches: []clientgotesting.PatchActionImpl{
				patchFinalizers(),
			},
			WantStatusUpdates: []clientgotesting.UpdateActionImpl{
				{
					Object: NewBroker(
						func(broker *eventing.Broker) {
							broker.Spec.Delivery = &eventingduck.DeliverySpec{
								DeadLetterSink: &duckv1.Destination{},
							}
						},
						reconcilertesting.WithInitBrokerConditions,
						TopicReady,
					),
				},
			},
		},
	}

	for i := range table {
		table[i].Name = table[i].Name + " - " + format
	}

	useTable(t, table, &configs)
}

func TestBrokerFinalizer(t *testing.T) {
	t.Parallel()

	for _, f := range Formats {
		brokerFinalization(t, f, *DefaultConfigs)
	}
}

func brokerFinalization(t *testing.T, format string, configs Configs) {

	testKey := fmt.Sprintf("%s/%s", BrokerNamespace, BrokerName)

	configs.DataPlaneConfigFormat = format

	table := TableTest{
		{
			Name: "Reconciled normal - no DLS",
			Objects: []runtime.Object{
				NewDeletedBroker(),
				NewConfigMapFromBrokers(&coreconfig.Brokers{
					Broker: []*coreconfig.Broker{
						{
							Id:        BrokerUUID,
							Topic:     GetTopic(),
							Namespace: BrokerNamespace,
							Name:      BrokerName,
						},
					},
					VolumeGeneration: 1,
				}, &configs),
			},
			Key: testKey,
			WantEvents: []string{
				Eventf(
					corev1.EventTypeNormal,
					Reconciled,
					fmt.Sprintf(`%s reconciled: "%s/%s"`, Broker, BrokerNamespace, BrokerName),
				),
			},
			WantUpdates: []clientgotesting.UpdateActionImpl{
				ConfigMapUpdate(&configs, &coreconfig.Brokers{
					Broker:           []*coreconfig.Broker{},
					VolumeGeneration: 1,
				}),
			},
		},
		{
			Name: "Reconciled normal - with DLS",
			Objects: []runtime.Object{
				NewDeletedBroker(
					WithDelivery(),
				),
				NewConfigMapFromBrokers(&coreconfig.Brokers{
					Broker: []*coreconfig.Broker{
						{
							Id:             BrokerUUID,
							Topic:          GetTopic(),
							DeadLetterSink: "http://test-service.test-service-namespace.svc.cluster.local/",
							Namespace:      BrokerNamespace,
							Name:           BrokerName,
						},
					},
					VolumeGeneration: 1,
				}, &configs),
			},
			Key: testKey,
			WantEvents: []string{
				Eventf(
					corev1.EventTypeNormal,
					Reconciled,
					fmt.Sprintf(`%s reconciled: "%s/%s"`, Broker, BrokerNamespace, BrokerName),
				),
			},
			WantUpdates: []clientgotesting.UpdateActionImpl{
				ConfigMapUpdate(&configs, &coreconfig.Brokers{
					VolumeGeneration: 1,
				}),
			},
		},
		{
			Name: "Failed to delete topic",
			Objects: []runtime.Object{
				NewDeletedBroker(),
				NewConfigMapFromBrokers(&coreconfig.Brokers{
					Broker: []*coreconfig.Broker{
						{
							Id:             BrokerUUID,
							Topic:          GetTopic(),
							DeadLetterSink: "http://test-service.test-service-namespace.svc.cluster.local/",
						},
					},
					VolumeGeneration: 1,
				}, &configs),
			},
			Key:     testKey,
			WantErr: true,
			WantEvents: []string{
				Eventf(
					corev1.EventTypeWarning,
					"InternalError",
					"failed to delete topic %s: %v",
					GetTopic(), deleteTopicError,
				),
			},
			WantUpdates: []clientgotesting.UpdateActionImpl{
				ConfigMapUpdate(&configs, &coreconfig.Brokers{
					VolumeGeneration: 1,
				}),
			},
			OtherTestData: map[string]interface{}{
				wantErrorOnDeleteTopic: deleteTopicError,
			},
		},
		{
			Name: "Failed to get config map",
			Objects: []runtime.Object{
				NewDeletedBroker(),
				&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: configs.DataPlaneConfigMapNamespace,
						Name:      configs.DataPlaneConfigMapName + "a",
					},
				},
			},
			Key:     testKey,
			WantErr: true,
			WantEvents: []string{
				Eventf(
					corev1.EventTypeWarning,
					"InternalError",
					"failed to get brokers and triggers config map %s: %v",
					configs.DataPlaneConfigMapAsString(), `configmaps "knative-eventing" not found`,
				),
			},
		},
		{
			Name: "Config map not readable",
			Objects: []runtime.Object{
				NewDeletedBroker(),
				NewConfigMap(&configs, []byte(`{"hello"-- "world"}`)),
			},
			Key:     testKey,
			WantErr: true,
			WantEvents: []string{
				Eventf(
					corev1.EventTypeWarning,
					"InternalError",
					`failed to get brokers and triggers: failed to unmarshal brokers and triggers: '{"hello"-- "world"}' - %v`,
					getUnmarshallableError(format),
				),
			},
		},
		{
			Name: "Reconciled normal - preserve config map previous state",
			Objects: []runtime.Object{
				NewDeletedBroker(),
				NewConfigMapFromBrokers(&coreconfig.Brokers{
					Broker: []*coreconfig.Broker{
						{
							Id:             "5384faa4-6bdf-428d-b6c2-d6f89ce1d44b",
							Topic:          "my-existing-topic-a",
							DeadLetterSink: "http://www.my-sink.com",
						},
						{
							Id:             BrokerUUID,
							Topic:          "my-existing-topic-b",
							DeadLetterSink: "http://www.my-sink.com",
						},
					},
					VolumeGeneration: 5,
				}, &configs),
			},
			Key: testKey,
			WantEvents: []string{
				Eventf(
					corev1.EventTypeNormal,
					Reconciled,
					fmt.Sprintf(`%s reconciled: "%s/%s"`, Broker, BrokerNamespace, BrokerName),
				),
			},
			WantUpdates: []clientgotesting.UpdateActionImpl{
				ConfigMapUpdate(&configs, &coreconfig.Brokers{
					Broker: []*coreconfig.Broker{
						{
							Id:             "5384faa4-6bdf-428d-b6c2-d6f89ce1d44b",
							Topic:          "my-existing-topic-a",
							DeadLetterSink: "http://www.my-sink.com",
						},
					},
					VolumeGeneration: 5,
				}),
			},
		},
		{
			Name: "Reconciled normal - topic doesn't exist",
			Objects: []runtime.Object{
				NewDeletedBroker(),
				NewConfigMapFromBrokers(&coreconfig.Brokers{
					Broker: []*coreconfig.Broker{
						{
							Id:             "5384faa4-6bdf-428d-b6c2-d6f89ce1d44b",
							Topic:          "my-existing-topic-a",
							DeadLetterSink: "http://www.my-sink.com",
						},
						{
							Id:             BrokerUUID,
							Topic:          "my-existing-topic-b",
							DeadLetterSink: "http://www.my-sink.com",
						},
					},
					VolumeGeneration: 5,
				}, &configs),
			},
			Key: testKey,
			WantEvents: []string{
				Eventf(
					corev1.EventTypeNormal,
					Reconciled,
					fmt.Sprintf(`%s reconciled: "%s/%s"`, Broker, BrokerNamespace, BrokerName),
				),
			},
			WantUpdates: []clientgotesting.UpdateActionImpl{
				ConfigMapUpdate(&configs, &coreconfig.Brokers{
					Broker: []*coreconfig.Broker{
						{
							Id:             "5384faa4-6bdf-428d-b6c2-d6f89ce1d44b",
							Topic:          "my-existing-topic-a",
							DeadLetterSink: "http://www.my-sink.com",
						},
					},
					VolumeGeneration: 5,
				}),
			},
			OtherTestData: map[string]interface{}{
				wantErrorOnDeleteTopic: sarama.ErrUnknownTopicOrPartition,
			},
		},
		{
			Name: "Reconciled normal - no broker found in config map",
			Objects: []runtime.Object{
				NewDeletedBroker(),
				NewConfigMapFromBrokers(&coreconfig.Brokers{
					Broker: []*coreconfig.Broker{
						{
							Id:             "5384faa4-6bdf-428d-b6c2-d6f89ce1d44b",
							Topic:          "my-existing-topic-a",
							DeadLetterSink: "http://www.my-sink.com",
						},
					},
					VolumeGeneration: 5,
				}, &configs),
			},
			Key: testKey,
			WantEvents: []string{
				Eventf(
					corev1.EventTypeNormal,
					Reconciled,
					fmt.Sprintf(`%s reconciled: "%s/%s"`, Broker, BrokerNamespace, BrokerName),
				),
			},
		},
	}

	for i := range table {
		table[i].Name = table[i].Name + " - " + format
	}

	useTable(t, table, &configs)
}

func useTable(t *testing.T, table TableTest, configs *Configs) {

	testCtx, cancel := context.WithCancel(context.Background())

	table.Test(t, NewFactory(configs, func(ctx context.Context, listers *Listers, configs *Configs, row *TableRow) controller.Reconciler {

		defaultTopicDetail := sarama.TopicDetail{
			NumPartitions:     DefaultNumPartitions,
			ReplicationFactor: DefaultReplicationFactor,
		}

		var onCreateTopicError error
		if want, ok := row.OtherTestData[wantErrorOnCreateTopic]; ok {
			onCreateTopicError = want.(error)
		}

		var onDeleteTopicError error
		if want, ok := row.OtherTestData[wantErrorOnDeleteTopic]; ok {
			onDeleteTopicError = want.(error)
		}

		clusterAdmin := &MockKafkaClusterAdmin{
			ExpectedTopicName:   fmt.Sprintf("%s%s-%s", TopicPrefix, BrokerNamespace, BrokerName),
			ExpectedTopicDetail: defaultTopicDetail,
			ErrorOnCreateTopic:  onCreateTopicError,
			ErrorOnDeleteTopic:  onDeleteTopicError,
			T:                   t,
		}

		reconciler := &Reconciler{
			Reconciler: &base.Reconciler{
				KubeClient:                  kubeclient.Get(ctx),
				PodLister:                   listers.GetPodLister(),
				DataPlaneConfigMapNamespace: configs.DataPlaneConfigMapNamespace,
				DataPlaneConfigMapName:      configs.DataPlaneConfigMapName,
				DataPlaneConfigFormat:       configs.DataPlaneConfigFormat,
				SystemNamespace:             configs.SystemNamespace,
			},
			KafkaClusterAdmin:            clusterAdmin,
			KafkaDefaultTopicDetails:     defaultTopicDetail,
			KafkaDefaultTopicDetailsLock: sync.RWMutex{},
			Configs:                      configs,
		}

		r := brokerreconciler.NewReconciler(
			ctx,
			logging.FromContext(ctx),
			fakeeventingclient.Get(ctx),
			listers.GetBrokerLister(),
			controller.GetEventRecorder(ctx),
			reconciler,
			kafka.BrokerClass,
		)

		reconciler.Resolver = resolver.NewURIResolver(ctx, func(name types.NamespacedName) {})

		// periodically update default topic details to simulate concurrency.
		go func() {

			ticker := time.NewTicker(10 * time.Millisecond)

			for {
				select {
				case <-testCtx.Done():
					return
				case <-ticker.C:
					reconciler.SetDefaultTopicDetails(defaultTopicDetail)
				}
			}
		}()

		return r
	}))

	cancel()
}

func TestConfigMapUpdate(t *testing.T) {

	NewClusterAdmin = func(addrs []string, conf *sarama.Config) (sarama.ClusterAdmin, error) {
		return MockKafkaClusterAdmin{}, nil
	}

	cm := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cmname",
			Namespace: "cmnamespace",
		},
		Data: map[string]string{
			DefaultTopicNumPartitionConfigMapKey:      "42",
			DefaultTopicReplicationFactorConfigMapKey: "3",
			BootstrapServersConfigMapKey:              "server1,server2",
		},
	}

	reconciler := Reconciler{}

	ctx, _ := SetupFakeContext(t)

	reconciler.ConfigMapUpdated(ctx)(&cm)

	assert.Equal(t, reconciler.KafkaDefaultTopicDetails, sarama.TopicDetail{
		NumPartitions:     42,
		ReplicationFactor: 3,
	})
	assert.NotNil(t, reconciler.KafkaClusterAdmin)
}

func patchFinalizers() clientgotesting.PatchActionImpl {
	action := clientgotesting.PatchActionImpl{}
	action.Name = BrokerName
	action.Namespace = BrokerNamespace
	patch := `{"metadata":{"finalizers":["` + finalizerName + `"],"resourceVersion":""}}`
	action.Patch = []byte(patch)
	return action
}

func getUnmarshallableError(format string) interface{} {
	if format == base.Protobuf {
		return "unexpected EOF"
	}
	return "invalid character '-' after object key"
}
