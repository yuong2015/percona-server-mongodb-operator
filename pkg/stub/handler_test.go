package stub

import (
	"context"
	"errors"
	"testing"

	"github.com/Percona-Lab/percona-server-mongodb-operator/internal/config"
	"github.com/Percona-Lab/percona-server-mongodb-operator/internal/mongod"
	"github.com/Percona-Lab/percona-server-mongodb-operator/internal/sdk/mocks"
	"github.com/Percona-Lab/percona-server-mongodb-operator/pkg/apis/psmdb/v1alpha1"
	wdMetrics "github.com/percona/mongodb-orchestration-tools/watchdog/metrics"

	"github.com/operator-framework/operator-sdk/pkg/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestHasBackupsEnabled(t *testing.T) {
	psmdb := &v1alpha1.PerconaServerMongoDB{
		Spec: v1alpha1.PerconaServerMongoDBSpec{
			Backup: &v1alpha1.BackupSpec{
				Tasks: []*v1alpha1.BackupTaskSpec{},
			},
		},
	}
	h := &Handler{}
	assert.False(t, h.hasBackupsEnabled(psmdb))

	psmdb.Spec.Backup.Tasks = append(psmdb.Spec.Backup.Tasks, &v1alpha1.BackupTaskSpec{
		Name:     t.Name(),
		Enabled:  true,
		Schedule: "* * * * *",
	})
	assert.True(t, h.hasBackupsEnabled(psmdb))
}

func TestHasReplsetsInitialized(t *testing.T) {
	psmdb := &v1alpha1.PerconaServerMongoDB{
		Status: v1alpha1.PerconaServerMongoDBStatus{
			Replsets: []*v1alpha1.ReplsetStatus{},
		},
	}
	h := &Handler{}

	assert.False(t, h.hasReplsetsInitialized(psmdb))

	psmdb.Status.Replsets = append(psmdb.Status.Replsets, &v1alpha1.ReplsetStatus{
		Initialized: true,
	})
	assert.True(t, h.hasReplsetsInitialized(psmdb))
}

func TestHandlerHandle(t *testing.T) {
	psmdb := &v1alpha1.PerconaServerMongoDB{
		ObjectMeta: metav1.ObjectMeta{
			Name:      t.Name(),
			Namespace: "test",
		},
		Spec: v1alpha1.PerconaServerMongoDBSpec{
			Secrets: &v1alpha1.SecretsSpec{
				Key:   config.DefaultKeySecretName,
				Users: config.DefaultUsersSecretName,
			},
			Replsets: []*v1alpha1.ReplsetSpec{
				{
					Name: config.DefaultReplsetName,
					Size: config.DefaultMongodSize,
					ResourcesSpec: &v1alpha1.ResourcesSpec{
						Limits: &v1alpha1.ResourceSpecRequirements{
							Cpu:     "1",
							Memory:  "1G",
							Storage: "1G",
						},
						Requests: &v1alpha1.ResourceSpecRequirements{
							Cpu:    "1",
							Memory: "1G",
						},
					},
				},
			},
			Mongod: &v1alpha1.MongodSpec{
				Net: &v1alpha1.MongodSpecNet{
					Port: 99999,
				},
			},
			Backup: &v1alpha1.BackupSpec{
				Enabled: true,
				Coordinator: &v1alpha1.BackupCoordinatorSpec{
					ResourcesSpec: &v1alpha1.ResourcesSpec{
						Limits: &v1alpha1.ResourceSpecRequirements{
							Cpu:     "100m",
							Memory:  "200m",
							Storage: "1G",
						},
						Requests: &v1alpha1.ResourceSpecRequirements{
							Cpu:    "100m",
							Memory: "200m",
						},
					},
				},
				Tasks: []*v1alpha1.BackupTaskSpec{},
			},
		},
	}
	event := sdk.Event{
		Object: psmdb,
	}

	client := &mocks.Client{}
	h := &Handler{
		client: client,
		serverVersion: &v1alpha1.ServerVersion{
			Platform: v1alpha1.PlatformKubernetes,
		},
		watchdogMetrics: wdMetrics.NewCollector(),
		watchdogQuit:    make(chan bool),
	}

	// test Handler with no existing stateful sets
	t.Run("new", func(t *testing.T) {
		client.On("Create", mock.AnythingOfType("*v1.Secret")).Return(nil)
		client.On("Create", mock.AnythingOfType("*v1.Service")).Return(nil)
		client.On("Create", mock.AnythingOfType("*v1.StatefulSet")).Return(nil)
		client.On("Update", mock.AnythingOfType("*v1.StatefulSet")).Return(nil)
		client.On("Update", mock.AnythingOfType("*v1alpha1.PerconaServerMongoDB")).Return(nil)
		client.On("Get", mock.AnythingOfType("*v1alpha1.PerconaServerMongoDB")).Return(nil)
		client.On("Get", mock.AnythingOfType("*v1.Secret")).Return(nil)
		client.On("Get", mock.AnythingOfType("*v1.StatefulSet")).Return(errors.New("not found error"))
		client.On("List",
			"test",
			mock.AnythingOfType("*v1.PodList"),
			mock.AnythingOfType("sdk.ListOption"),
		).Return(nil)
		client.On("List",
			"test",
			mock.AnythingOfType("*v1.ServiceList"),
			mock.AnythingOfType("sdk.ListOption"),
		).Return(nil)

		assert.NoError(t, h.Handle(context.TODO(), event))
		assert.Nil(t, h.watchdog)
		client.AssertExpectations(t)
	})

	// test Handler with existing stateful set (mocked)
	t.Run("update", func(t *testing.T) {
		client.On("Get", mock.AnythingOfType("*v1.StatefulSet")).Return(nil).Run(func(args mock.Arguments) {
			set := args.Get(0).(*appsv1.StatefulSet)
			set.Spec = appsv1.StatefulSetSpec{
				Replicas: &config.DefaultMongodSize,
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name: mongod.MongodContainerName,
							},
						},
					},
				},
			}
		})
		client.On("Update", mock.AnythingOfType("*v1.StatefulSet")).Return(nil)
		assert.NoError(t, h.Handle(context.TODO(), event))
		client.AssertExpectations(t)
	})

	// test watchdog is started if 1+ replsets are initialized
	t.Run("watchdog-start", func(t *testing.T) {
		psmdb.Status.Replsets = []*v1alpha1.ReplsetStatus{
			{
				Name:        t.Name(),
				Initialized: true,
			},
		}
		assert.NoError(t, h.Handle(context.TODO(), event))
		assert.NotNil(t, h.watchdog)

		// check last call was a Create with a corev1.Service object:
		calls := len(client.Calls)
		lastCall := client.Calls[calls-1]
		assert.Equal(t, "List", lastCall.Method)
		assert.IsType(t, "", lastCall.Arguments.Get(0))
	})

	// test watchdog is stopped by a 'Deleted' SDK event
	t.Run("watchdog-stop", func(t *testing.T) {
		event.Deleted = true
		assert.NoError(t, h.Handle(context.TODO(), event))
		assert.Nil(t, h.watchdog)
	})
}
