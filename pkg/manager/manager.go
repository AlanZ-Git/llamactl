package manager

import (
	"context"
	"errors"
	"fmt"
	"llamactl/pkg/config"
	"llamactl/pkg/database"
	"llamactl/pkg/instance"
	"llamactl/pkg/resourcepolicy"
	"llamactl/pkg/resources"
	"log"
	"sort"
	"sync"
	"time"
)

// InstanceManager defines the interface for managing instances of the llama server.
type InstanceManager interface {
	ListInstances() ([]*instance.Instance, error)
	CreateInstance(name string, options *instance.Options) (*instance.Instance, error)
	GetInstance(name string) (*instance.Instance, error)
	UpdateInstance(name string, options *instance.Options) (*instance.Instance, error)
	DeleteInstance(name string) error
	StartInstance(name string) (*instance.Instance, error)
	AtMaxRunning() bool
	CountRunningInGroup(group string) int
	StopInstance(name string) (*instance.Instance, error)
	EvictLRUInstance(group string) error
	RestartInstance(name string) (*instance.Instance, error)
	GetInstanceLogs(name string, numLines int) (string, error)
	Shutdown()
}

type instanceManager struct {
	// Components (each with own synchronization)
	registry  *instanceRegistry
	ports     *portAllocator
	db        database.InstanceStore
	remote    *remoteManager
	lifecycle *lifecycleManager

	// Configuration
	globalConfig   *config.AppConfig
	startupBlocked bool

	// Synchronization
	instanceLocks sync.Map // map[string]*sync.Mutex - per-instance locks for concurrent operations
	shutdownOnce  sync.Once
	controlOnce   sync.Once
	control       *controlLedger
	resources     *resources.Sampler
}

// New creates a new instance of InstanceManager with dependency injection.
func New(globalConfig *config.AppConfig, db database.InstanceStore) InstanceManager {
	policy, err := resourcepolicy.Open(globalConfig.DataDir)
	if policy == nil {
		panic(fmt.Errorf("cannot initialize owner policy: %w", err))
	}
	if err != nil {
		log.Printf("Owner policy blocked: %v", err)
	}
	globalConfig.ResourcePolicy = policy

	if globalConfig.Instances.TimeoutCheckInterval <= 0 {
		globalConfig.Instances.TimeoutCheckInterval = 5 // Default to 5 minutes if not set
	}

	// Initialize components
	registry := newInstanceRegistry()

	// Initialize port allocator
	portRange := globalConfig.Instances.PortRange
	ports := newPortAllocator(portRange[0], portRange[1])

	// Initialize remote manager
	remote := newRemoteManager(globalConfig.Nodes, 30*time.Second)

	// Create manager instance
	im := &instanceManager{
		registry:       registry,
		ports:          ports,
		db:             db,
		remote:         remote,
		globalConfig:   globalConfig,
		startupBlocked: !policy.Snapshot().AllowAI,
	}

	policy.SetReleaseRunner(im.releaseOwned)

	// Initialize lifecycle manager (needs reference to manager for Stop/Evict operations)
	checkInterval := time.Duration(globalConfig.Instances.TimeoutCheckInterval) * time.Minute
	im.lifecycle = newLifecycleManager(registry, im, checkInterval, true)

	// Load existing instances from disk
	if err := im.loadInstances(); err != nil {
		log.Printf("Error loading instances: %v", err)
	}

	// Start the lifecycle manager
	im.lifecycle.start()

	collector := resources.NewCollector(im.collectOwnedResources)
	im.resources = resources.NewSampler(collector.Collect, im.resourcesIdle)
	im.resources.Start()

	return im
}

// persistInstance saves an instance using the persistence layer
func (im *instanceManager) persistInstance(inst *instance.Instance) error {
	return im.db.Save(inst)
}

func (im *instanceManager) Shutdown() {
	im.shutdownOnce.Do(func() {
		if im.resources != nil {
			im.resources.Stop()
		}
		im.shutdownControl()
		// 1. Stop lifecycle manager (stops timeout checker)
		im.lifecycle.stop()

		// Include protocol generations interrupted while shutting down: registry
		// Running alone is not proof that an owned tree has exited.
		running := im.registry.list()

		// 3. Stop local instances concurrently
		var wg sync.WaitGroup
		for _, inst := range running {
			if !inst.ProtocolEnabled() && !inst.IsRunning() {
				continue
			}
			if inst.IsRemote() {
				continue // Skip remote instances
			}
			wg.Add(1)
			go func(inst *instance.Instance) {
				defer wg.Done()
				log.Printf("Stopping instance %s...\n", inst.Name)
				if inst.ProtocolEnabled() {
					s := inst.ControlSnapshot()
					lease, err := inst.BeginControl(s.BootID, s.Revision, false, true)
					if err == nil {
						ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
						err = lease.StopControl(ctx)
						cancel()
						lease.Close()
					}
					if err != nil {
						log.Printf("Control shutdown failed for %s: %v", inst.Name, err)
					}
					return
				}
				if err := inst.Stop(); err != nil {
					log.Printf("Error stopping instance %s: %v\n", inst.Name, err)
				}
			}(inst)
		}
		wg.Wait()
		log.Println("Instance shutdown attempts completed.")
	})
}

// loadInstances restores all instances from the persistence layer
func (im *instanceManager) loadInstances() error {
	bootPermit, _ := im.globalConfig.ResourcePolicy.Acquire()
	launched := false
	defer func() {
		if !launched {
			bootPermit.Close()
		}
	}()
	// Load all instances from persistence
	instances, err := im.db.LoadAll()
	if err != nil {
		return fmt.Errorf("failed to load instances: %w", err)
	}

	if len(instances) == 0 {
		return nil
	}

	var failures []error
	loaded := 0
	// Process each loaded instance
	for _, persistedInst := range instances {
		if err := im.loadInstance(persistedInst); err != nil {
			failures = append(failures, fmt.Errorf("load instance %s: %w", persistedInst.Name, err))
			continue
		}
		loaded++
	}

	log.Printf("Loaded %d instances from persistence", loaded)

	// Only successfully prepared instances are eligible for autostart.
	launched = true
	go im.autoStartInstancesWithPermit(bootPermit)

	return errors.Join(failures...)
}

// loadInstance loads a single persisted instance and adds it to the registry
func (im *instanceManager) loadInstance(persistedInst *instance.Instance) error {
	name := persistedInst.Name
	options := persistedInst.GetOptions()

	// Check if this is a remote instance (local node not in the Nodes set)
	var isRemote bool
	var nodeName string
	if options != nil {
		if _, isLocal := options.Nodes[im.globalConfig.LocalNode]; !isLocal && len(options.Nodes) > 0 {
			// Get the first node from the set
			for node := range options.Nodes {
				nodeName = node
				isRemote = true
				break
			}
		}
	}

	var statusCallback func(oldStatus, newStatus instance.Status)
	if !isRemote {
		// Only set status callback for local instances
		statusCallback = func(oldStatus, newStatus instance.Status) {
			im.onStatusChange(name, oldStatus, newStatus)
		}
	}

	// Create new inst using NewInstance (handles validation, defaults, setup)
	constructor := instance.NewChecked
	if (im.startupBlocked || (options != nil && options.Control != nil)) && !isRemote {
		constructor = instance.NewDescriptor
	}
	inst, err := constructor(name, im.globalConfig, options, statusCallback)
	if err != nil {
		return fmt.Errorf("prepare persisted instance: %w", err)
	}

	// Restore persisted fields that NewInstance doesn't set
	if inst.ProtocolEnabled() {
		inst.InitializeControl(im.initControl().boot)
	}
	inst.ID = persistedInst.ID
	inst.Created = persistedInst.Created
	if inst.IsControlled() || (im.startupBlocked && !isRemote) {
		inst.SetStatus(instance.Stopped)
	} else {
		inst.SetStatus(persistedInst.GetStatus())
	}

	// Handle remote instance mapping
	if isRemote {
		// Map instance to node in remote manager
		if err := im.remote.setInstanceNode(name, nodeName); err != nil {
			return fmt.Errorf("failed to set instance node: %w", err)
		}
	} else {
		// Allocate port for local instances
		if inst.GetPort() > 0 {
			port := inst.GetPort()
			if err := im.ports.allocateSpecific(port, name); err != nil {
				return fmt.Errorf("port conflict: instance %s wants port %d which is already in use: %w", name, port, err)
			}
		}
	}

	// Add instance to registry
	if err := im.registry.add(inst); err != nil {
		if !isRemote && inst.GetPort() > 0 {
			im.ports.release(inst.GetPort())
		}
		if isRemote {
			im.remote.removeInstance(name)
		}
		return fmt.Errorf("failed to add instance to registry: %w", err)
	}

	return nil
}

// autoStartInstances starts instances that were running when persisted and have auto-restart enabled
// For instances with auto-restart disabled, it sets their status to Stopped
func (im *instanceManager) autoStartInstances() {
	permit, _ := im.globalConfig.ResourcePolicy.Acquire()
	im.autoStartInstancesWithPermit(permit)
}

func (im *instanceManager) autoStartInstancesWithPermit(permit *resourcepolicy.Permit) {
	defer permit.Close()
	instances := im.registry.list()

	var instancesToStart []*instance.Instance
	var instancesToStop []*instance.Instance

	for _, inst := range instances {
		if inst.IsControlled() || (im.startupBlocked && !inst.IsRemote()) {
			continue
		}
		if inst.IsRunning() && // Was running when persisted
			inst.GetOptions() != nil &&
			inst.GetOptions().AutoRestart != nil {
			if *inst.GetOptions().AutoRestart {
				instancesToStart = append(instancesToStart, inst)
			} else {
				// Instance was running but auto-restart is disabled, mark as stopped
				instancesToStop = append(instancesToStop, inst)
			}
		}
	}

	// Stop instances that have auto-restart disabled
	for _, inst := range instancesToStop {
		log.Printf("Instance %s was running but auto-restart is disabled, setting status to stopped", inst.Name)
		im.markAutomaticStopped(inst)
	}

	// Start instances that have auto-restart enabled. Enforce group and
	// global limits: start the most recently used first and leave the rest
	// stopped (they start on demand). Previously this started every
	// persisted-running instance, which could boot two large models at
	// once and spill VRAM into system RAM.
	sort.Slice(instancesToStart, func(i, j int) bool {
		return instancesToStart[i].LastRequestTime() > instancesToStart[j].LastRequestTime()
	})

	// Clear the persisted "running" state of every candidate *before* the limit
	// checks below. loadInstance restored these instances as running, so the
	// registry still reports them as running; counting that phantom occupancy
	// skips candidates this loop has not reached yet and starts fewer instances
	// than the limits allow (with a group limit, possibly none). Each successful
	// Start() marks its own instance running again, so earlier starts in this
	// loop — and concurrent on-demand starts — still count against the limits.
	for _, inst := range instancesToStart {
		im.markAutomaticStopped(inst)
	}

	limits := im.globalConfig.Instances
	for _, inst := range instancesToStart {
		group := ""
		if inst.GetOptions() != nil {
			group = inst.GetOptions().Group
		}

		// Enforce limits against live registry state (not loop-local tallies):
		// on-demand starts made concurrently during boot are visible here, so
		// the loop can't approve a start past a limit that is already reached.
		if group != "" {
			if limit, ok := limits.GroupLimits[group]; ok && limit > 0 &&
				im.CountRunningInGroup(group) >= limit {
				log.Printf("Instance %s: group %s already at limit (%d); leaving stopped (will start on demand)", inst.Name, group, limit)
				continue
			}
		}
		if limits.MaxRunningInstances > 0 && im.AtMaxRunning() {
			log.Printf("Instance %s: at global running limit; leaving stopped (will start on demand)", inst.Name)
			continue
		}

		log.Printf("Auto-starting instance %s", inst.Name)

		// Check if this is a remote instance
		if node, exists := im.remote.getNodeForInstance(inst.Name); exists && node != nil {
			// Remote instance - use remote manager with context
			ctx := context.Background()
			if _, err := im.remote.startInstance(ctx, node, inst.Name); err != nil {
				log.Printf("Failed to auto-start remote instance %s: %v", inst.Name, err)
				continue
			}
		} else {
			if im.startupBlocked || (im.globalConfig.ResourcePolicy != nil && permit == nil) {
				continue
			}
			if err := permit.Check(); err != nil {
				continue
			}
			// Local instance - call Start() directly
			if err := inst.StartAutomatic(); err != nil {
				log.Printf("Failed to auto-start instance %s: %v", inst.Name, err)
				continue
			}
		}
	}
}

// markAutomaticStopped rechecks mode under admission before resetting restored
// legacy state. A concurrent stopped-mode transition must not reset a newly
// explicitly started controlled instance.
func (im *instanceManager) markAutomaticStopped(inst *instance.Instance) {
	lease, err := inst.BeginMaintenance()
	if err != nil {
		return
	}
	defer lease.Close()
	if inst.IsControlled() {
		return
	}
	inst.SetStatus(instance.Stopped)
	im.registry.markStopped(inst.Name)
}

func (im *instanceManager) onStatusChange(name string, _, newStatus instance.Status) {
	if newStatus == instance.Running {
		im.registry.markRunning(name)
	} else {
		im.registry.markStopped(name)
	}
}

// getNodeForInstance returns the node configuration for a remote instance
// Returns nil if the instance is not remote or the node is not found
func (im *instanceManager) getNodeForInstance(inst *instance.Instance) *config.NodeConfig {
	if !inst.IsRemote() {
		return nil
	}

	// Check if we have a node mapping in remote manager
	if nodeConfig, exists := im.remote.getNodeForInstance(inst.Name); exists {
		return nodeConfig
	}

	return nil
}

// lockInstance returns the lock for a specific instance, creating one if needed.
// This allows concurrent operations on different instances while preventing
// concurrent operations on the same instance.
func (im *instanceManager) lockInstance(name string) *sync.Mutex {
	lock, _ := im.instanceLocks.LoadOrStore(name, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// unlockAndCleanup unlocks the instance lock and removes it from the map.
// This should only be called when deleting an instance to prevent memory leaks.
func (im *instanceManager) unlockAndCleanup(name string) {
	if lock, ok := im.instanceLocks.Load(name); ok {
		lock.(*sync.Mutex).Unlock()
		im.instanceLocks.Delete(name)
	}
}
