package controller

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	runtimejson "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1informers "k8s.io/client-go/informers/core/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	coreapi "k8s.io/kubernetes/pkg/apis/core"

	securityv1 "github.com/openshift/api/security/v1"
	securityinternalv1 "github.com/openshift/api/securityinternal/v1"
	securityv1client "github.com/openshift/client-go/securityinternal/clientset/versioned/typed/securityinternal/v1"
	"github.com/openshift/cluster-policy-controller/pkg/security/mcs"
	"github.com/openshift/cluster-policy-controller/pkg/security/uidallocator"
	"github.com/openshift/library-go/pkg/security/uid"
)

const (
	controllerName    = "namespace-security-allocation-controller"
	rangeName         = "scc-uid"
	periodicRepairKey = "__internal/periodicRepair"
)

// NamespaceSCCAllocationController allocates uids/labels for namespaces
type NamespaceSCCAllocationController struct {
	requiredUIDRange          *uid.Range
	mcsAllocator              MCSAllocationFunc
	nsLister                  corev1listers.NamespaceLister
	nsListerSynced            cache.InformerSynced
	currentUIDRangeAllocation *securityinternalv1.RangeAllocation

	namespaceClient       corev1client.NamespaceInterface
	rangeAllocationClient securityv1client.RangeAllocationsGetter

	queue workqueue.RateLimitingInterface

	encoder runtime.Encoder

	eventRecorder record.EventRecorder
}

func NewNamespaceSCCAllocationController(
	namespaceInformer corev1informers.NamespaceInformer,
	client corev1client.NamespaceInterface,
	rangeAllocationClient securityv1client.RangeAllocationsGetter,
	requiredUIDRange *uid.Range,
	mcs MCSAllocationFunc,
	eventsBroadcaster record.EventBroadcaster,
) *NamespaceSCCAllocationController {

	scheme := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(scheme))
	codecs := serializer.NewCodecFactory(scheme)
	jsonSerializer := runtimejson.NewSerializer(runtimejson.DefaultMetaFactory, scheme, scheme, false)
	encoder := codecs.WithoutConversion().EncoderForVersion(jsonSerializer, corev1.SchemeGroupVersion)

	c := &NamespaceSCCAllocationController{
		requiredUIDRange:      requiredUIDRange,
		mcsAllocator:          mcs,
		namespaceClient:       client,
		rangeAllocationClient: rangeAllocationClient,
		nsLister:              namespaceInformer.Lister(),
		nsListerSynced:        namespaceInformer.Informer().HasSynced,
		queue:                 workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), controllerName),
		encoder:               encoder,
		eventRecorder:         eventsBroadcaster.NewRecorder(scheme, corev1.EventSource{Component: controllerName}),
	}

	namespaceInformer.Informer().AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc: c.enqueueNamespace,
			UpdateFunc: func(oldObj, newObj interface{}) {
				c.enqueueNamespace(newObj)
			},
		},
		10*time.Minute,
	)
	return c
}

// Run starts the workers for this controller.
func (c *NamespaceSCCAllocationController) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	defer klog.V(1).Infof("Shutting down")

	// Wait for the stores to fill
	if !cache.WaitForCacheSync(stopCh, c.nsListerSynced) {
		return
	}

	klog.V(1).Infof("Repairing SCC UID Allocations")
	if err := c.WaitForRepair(stopCh); err != nil {
		// this is consistent with previous behavior
		klog.Fatal(err)
	}
	klog.V(1).Infof("Repair complete")

	c.AddNextPeriodicRepair()

	go c.worker()
	<-stopCh
}

// syncNamespace will sync the namespace with the given key.
// This function is not meant to be invoked concurrently with the same key.
func (c *NamespaceSCCAllocationController) syncNamespace(key string) error {
	ns, err := c.nsLister.Get(key)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, ok := ns.Annotations[securityv1.UIDRangeAnnotation]; ok {
		return nil
	}

	return c.allocate(ns)
}

func (c *NamespaceSCCAllocationController) allocate(ns *corev1.Namespace) error {
	// unless we affirmatively succeed, clear the local state to try again
	success := false
	defer func() {
		if success {
			return
		}
		c.currentUIDRangeAllocation = nil
	}()

	// if we don't have the current state, go get it
	if c.currentUIDRangeAllocation == nil {
		newRange, err := c.rangeAllocationClient.RangeAllocations().Get(context.TODO(), rangeName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		c.currentUIDRangeAllocation = newRange
	}

	// do uid allocation.  We reserve the UID we want first, lock it in etcd, then update the namespace.
	// We allocate by reading in a giant bit int bitmap (one bit per offset location), finding the next step,
	// then calculating the offset location
	uidRange, err := uid.ParseRange(c.currentUIDRangeAllocation.Range)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(*uidRange, *c.requiredUIDRange) {
		return fmt.Errorf("conflicting UID range; expected %#v, got %#v", *c.requiredUIDRange, *uidRange)
	}
	allocatedBitMapInt := big.NewInt(0).SetBytes(c.currentUIDRangeAllocation.Data)
	bitIndex, found := allocateNextContiguousBit(allocatedBitMapInt, int(uidRange.Size()))
	if !found {
		return fmt.Errorf("uid range exceeded")
	}
	allocatedBitMapInt = allocatedBitMapInt.SetBit(allocatedBitMapInt, bitIndex, 1)
	newRangeAllocation := c.currentUIDRangeAllocation.DeepCopy()
	newRangeAllocation.Data = allocatedBitMapInt.Bytes()

	actualRangeAllocation, err := c.rangeAllocationClient.RangeAllocations().Update(context.TODO(), newRangeAllocation, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	c.currentUIDRangeAllocation = actualRangeAllocation

	block, ok := uidRange.BlockAt(uint32(bitIndex))
	if !ok {
		return fmt.Errorf("%d not in range", bitIndex)
	}

	// Now modify the namespace
	nsCopy := ns.DeepCopy()
	if nsCopy.Annotations == nil {
		nsCopy.Annotations = make(map[string]string)
	}
	nsCopy.Annotations[securityv1.UIDRangeAnnotation] = block.String()
	nsCopy.Annotations[securityv1.SupplementalGroupsAnnotation] = block.String()
	if _, ok := nsCopy.Annotations[securityv1.MCSAnnotation]; !ok {
		if label := c.mcsAllocator(block); label != nil {
			nsCopy.Annotations[securityv1.MCSAnnotation] = label.String()
		}
	}
	nsCopyBytes, err := runtime.Encode(c.encoder, nsCopy)
	if err != nil {
		return err
	}
	nsBytes, err := runtime.Encode(c.encoder, ns)
	if err != nil {
		return err
	}
	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(nsBytes, nsCopyBytes, &corev1.Namespace{})
	if err != nil {
		return err
	}
	// use patch here not to conflict with other actors
	_, err = c.namespaceClient.Patch(context.TODO(), ns.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// emit event once per namespace.  There aren't many of these, but it will let us know how long it takes from namespace creation
	// until the SCC ranges are created.  There is a suspicion that this takes a while.
	c.eventRecorder.Eventf(&corev1.ObjectReference{
		Kind:      "Namespace",
		Namespace: ns.Name,
		Name:      ns.Name,
	}, corev1.EventTypeNormal, "CreatedSCCRanges", "created SCC ranges")

	success = true
	return nil
}

// allocateNextContiguousBit finds a free bit in the int and returns which one it is and whether it succeeded
func allocateNextContiguousBit(allocated *big.Int, max int) (int, bool) {
	for i := 0; i < max; i++ {
		if allocated.Bit(i) == 0 {
			return i, true
		}
	}
	return 0, false
}

func (c *NamespaceSCCAllocationController) AddNextPeriodicRepair() {
	// For this controller to work correctly we need to ensure a periodic repair
	// of all the range allocations. For that we are adding an artificial key
	// which will trigger that every 8 hours.
	delta := time.Duration(8 * time.Hour)
	c.queue.AddAfter(periodicRepairKey, delta)
	klog.V(1).Infof("Adding next periodic repair on %s", time.Now().Add(delta))
}

func (c *NamespaceSCCAllocationController) WaitForRepair(stopCh <-chan struct{}) error {
	return wait.PollImmediate(10*time.Second, 5*time.Minute, func() (bool, error) {
		select {
		case <-stopCh:
			return true, nil
		default:
		}
		err := c.Repair()
		if err == nil {
			return true, nil
		}
		utilruntime.HandleError(err)
		return false, nil
	})
}

func (c *NamespaceSCCAllocationController) Repair() error {
	// TODO: (per smarterclayton) if Get() or List() is a weak consistency read,
	// or if they are executed against different leaders,
	// the ordering guarantee required to ensure no item is allocated twice is violated.
	// List must return a ResourceVersion higher than the etcd index Get,
	// and the release code must not release items that have allocated but not yet been created
	// See https://github.com/kubernetes/kubernetes/issues/8295

	// get the curr so we have a resourceVersion to pin to
	uidRange, err := c.rangeAllocationClient.RangeAllocations().Get(context.TODO(), rangeName, metav1.GetOptions{})
	needCreate := apierrors.IsNotFound(err)
	if err != nil && !needCreate {
		return err
	}
	if needCreate {
		uidRange = &securityinternalv1.RangeAllocation{ObjectMeta: metav1.ObjectMeta{Name: rangeName}}
	}

	uids, err := uidallocator.NewInMemory(c.requiredUIDRange)
	if err != nil {
		return err
	}
	nsList, err := c.nsLister.List(labels.Everything())
	if err != nil {
		return err
	}
	for _, ns := range nsList {
		value, ok := ns.Annotations[securityv1.UIDRangeAnnotation]
		if !ok {
			continue
		}
		block, err := uid.ParseBlock(value)
		if err != nil {
			continue
		}

		switch err := uids.Allocate(block); err {
		case nil:
		case uidallocator.ErrNotInRange, uidallocator.ErrAllocated:
			continue
		case uidallocator.ErrFull:
			msg := fmt.Sprintf("the UID range %s is full; you must widen the range in order to allocate more UIDs", c.requiredUIDRange)
			c.eventRecorder.Event(uidRange, corev1.EventTypeWarning, "UIDRangeFull", msg)
			return errors.New(msg)
		default:
			return fmt.Errorf("unable to allocate UID block %s for namespace %s due to an unknown error, exiting: %v", block, ns.Name, err)
		}
	}

	newRangeAllocation := &coreapi.RangeAllocation{}
	if err := uids.Snapshot(newRangeAllocation); err != nil {
		return err
	}
	uidRange.Range = newRangeAllocation.Range
	uidRange.Data = newRangeAllocation.Data

	if needCreate {
		if _, err := c.rangeAllocationClient.RangeAllocations().Create(context.TODO(), uidRange, metav1.CreateOptions{}); err != nil {
			return err
		}
		return nil
	}

	if _, err := c.rangeAllocationClient.RangeAllocations().Update(context.TODO(), uidRange, metav1.UpdateOptions{}); err != nil {
		return err
	}

	return nil
}

type MCSAllocationFunc func(uid.Block) *mcs.Label

// DefaultMCSAllocation returns a label from the MCS range that matches the offset
// within the overall range. blockSize must be a positive integer representing the
// number of labels to jump past in the category space (if 1, range == label, if 2
// each range will have two labels).
func DefaultMCSAllocation(from *uid.Range, to *mcs.Range, blockSize int) MCSAllocationFunc {
	return func(block uid.Block) *mcs.Label {
		ok, offset := from.Offset(block)
		if !ok {
			return nil
		}
		if blockSize > 0 {
			offset = offset * uint32(blockSize)
		}
		label, _ := to.LabelAt(uint64(offset))
		return label
	}
}

func (c *NamespaceSCCAllocationController) enqueueNamespace(obj interface{}) {
	ns, ok := obj.(*corev1.Namespace)
	if !ok {
		return
	}
	c.queue.Add(ns.Name)
}

// worker runs a worker thread that just dequeues items, processes them, and marks them done.
// It enforces that the syncHandler is never invoked concurrently with the same key.
func (c *NamespaceSCCAllocationController) worker() {
	for c.work() {
	}
}

// work returns true if the worker thread should continue
func (c *NamespaceSCCAllocationController) work() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer func() {
		if key != periodicRepairKey {
			c.queue.Done(key)
		}
	}()

	if key == periodicRepairKey {
		if err := c.Repair(); err != nil {
			utilruntime.HandleError(fmt.Errorf("error during periodic repair: %v", err))
		}
		c.queue.Done(key)
		c.AddNextPeriodicRepair()
		return true
	}

	if err := c.syncNamespace(key.(string)); err == nil {
		// this means the request was successfully handled.  We should "forget" the item so that any retry
		// later on is reset
		c.queue.Forget(key)
	} else {
		// if we had an error it means that we didn't handle it, which means that we want to requeue the work
		utilruntime.HandleError(fmt.Errorf("error syncing namespace, it will be retried: %v", err))
		c.queue.AddRateLimited(key)
	}
	return true
}
