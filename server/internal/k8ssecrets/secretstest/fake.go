// Package secretstest is a fake Kubernetes clientset that behaves like the API server for the
// parts credential Secrets rely on: it assigns UIDs on create and enforces delete preconditions.
// Tests use it in place of a cluster.
package secretstest

import (
	"fmt"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// Client is a fake clientset and a log of the Secret deletes it saw.
type Client struct {
	*fake.Clientset
	// Deletes records "name@uid" for each delete request, whether or not it was allowed.
	Deletes []string
}

// New returns an empty fake cluster.
func New() *Client {
	c := &Client{Clientset: fake.NewClientset()}
	var n atomic.Int64
	c.PrependReactor("create", "secrets", func(a clienttesting.Action) (bool, runtime.Object, error) {
		sec := a.(clienttesting.CreateAction).GetObject().(*corev1.Secret)
		sec.UID = types.UID(fmt.Sprintf("uid-%d", n.Add(1)))
		return false, nil, nil // the tracker stores it
	})
	c.PrependReactor("delete", "secrets", func(a clienttesting.Action) (bool, runtime.Object, error) {
		del := a.(clienttesting.DeleteAction)
		pre := del.GetDeleteOptions().Preconditions
		uid := ""
		if pre != nil && pre.UID != nil {
			uid = string(*pre.UID)
		}
		c.Deletes = append(c.Deletes, del.GetName()+"@"+uid)
		obj, err := c.Tracker().Get(a.GetResource(), del.GetNamespace(), del.GetName())
		if err != nil {
			return false, nil, nil // not found: the tracker answers
		}
		if pre != nil && pre.UID != nil && obj.(*corev1.Secret).UID != *pre.UID {
			return true, nil, apierrors.NewConflict(a.GetResource().GroupResource(), del.GetName(),
				fmt.Errorf("precondition failed: UID in precondition: %s, UID in object meta: %s", uid, obj.(*corev1.Secret).UID))
		}
		return false, nil, nil
	})
	return c
}

// UID returns the UID of a stored Secret, or "" if it does not exist.
func (c *Client) UID(namespace, name string) string {
	obj, err := c.Tracker().Get(corev1.SchemeGroupVersion.WithResource("secrets"), namespace, name)
	if err != nil {
		return ""
	}
	return string(obj.(*corev1.Secret).UID)
}

// Replace deletes a Secret and creates another under the same name, as an out-of-band change
// would, so it gets a new UID.
func (c *Client) Replace(namespace, name string) error {
	old, err := c.Tracker().Get(corev1.SchemeGroupVersion.WithResource("secrets"), namespace, name)
	if err != nil {
		return err
	}
	sec := old.(*corev1.Secret).DeepCopy()
	sec.UID = ""
	sec.ResourceVersion = ""
	if err := c.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("secrets"), namespace, name); err != nil {
		return err
	}
	sec.UID = types.UID(fmt.Sprintf("replaced-%s", name))
	return c.Tracker().Create(corev1.SchemeGroupVersion.WithResource("secrets"), sec, namespace)
}
