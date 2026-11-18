package sidecar

import (
	"context"
	"fmt"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestListDoesNotRaceCreate walks the id map from List while Create stores
// a new id. List used to range the live map after dropping the lock.
func TestListDoesNotRaceCreate(t *testing.T) {
	d := &Docker{cli: &scriptedDocker{}, ids: map[string]map[string]string{}}
	pod, _ := restartPod()
	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				c := corev1.Container{Name: fmt.Sprintf("c-%d-%d", n, i), Image: "busybox"}
				if err := d.Create(context.Background(), pod, c, ""); err != nil {
					t.Errorf("create: %v", err)
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				if _, err := d.List(context.Background(), pod.Namespace, pod.Name); err != nil {
					t.Errorf("list: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
