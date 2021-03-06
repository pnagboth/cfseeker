package seeker

import (
	"fmt"
	"sync"
	"time"

	"github.com/cloudfoundry-community/gogobosh"
	"github.com/starkandwayne/goutils/log"
)

//VMCache contains the fields needed for caching VM information
type VMCache struct {
	data        map[string]*VMInfo
	deployments map[string]*deploymentEntry //not nil if cached
	ttl         time.Duration
	lock        sync.Mutex
}

type deploymentEntry struct {
	hosts    []string //list of ips cached under this deployment
	cachedAt time.Time
}

//VMInfo contains information about a VM...
type VMInfo struct {
	JobName        string
	DeploymentName string
	IP             string
	Index          int
}

func newVMCache() *VMCache {
	return &VMCache{
		data:        map[string]*VMInfo{},
		deployments: map[string]*deploymentEntry{},
		ttl:         -1,
	}
}

// GetVMWithIP searches the BOSH director for the VM with the IP you've given
// An error is returned if a problem is encountered while contacting the BOSH
// director. If the VM simply could not be found in the configured deployments,
// no error is returned, but vm will be nil.
func (s *Seeker) GetVMWithIP(ip string) (vm *VMInfo, err error) {
	log.Debugf("Getting VM with IP (%s)", ip)
	ip, err = canonizeIP(ip)
	if err != nil {
		return
	}

	//Attempt to get from cache
	vm = s.getFromCache(ip)
	if vm != nil {
		log.Debugf("Cache HIT for VM with IP (%s)", ip)
		return
	}
	log.Debugf("Cache MISS for VM with IP (%s)", ip)

	//If we're here, we need to (try to) fetch the VM from BOSH
	vm, err = s.cacheUntil(ip)
	if err != nil {
		err = fmt.Errorf("Error fetching VMs: %s", err.Error())
		return
	}
	if vm != nil {
		log.Debugf("Found VM with IP (%s) after fetching", ip)
		return
	}

	log.Debugf("Could not find VM with IP (%s). Refreshing cache...", ip)
	s.InvalidateAll()

	//refresh cache
	vm, err = s.cacheUntil(ip)
	if err != nil {
		err = fmt.Errorf("Error fetching VMs: %s", err.Error())
	}

	if vm != nil {
		log.Debugf("Found VM with IP (%s) after cache refresh", ip)
	} else {
		log.Debugf("Could not find VM with IP (%s). Are your deployments configured correctly?", ip)
	}
	return
}

//SetTTL sets how long (in seconds) before cache entries are wiped.
func (s *Seeker) SetTTL(ttl time.Duration) {
	log.Debugf("Setting BOSH VM cache TTL (%s)", ttl)
	s.vmcache.ttl = ttl
}

func (s *Seeker) getFromCache(host string) (ret *VMInfo) {
	s.acquireLock()
	defer s.releaseLock()
	c := s.vmcache
	if ret = c.data[host]; ret != nil {
		dep := c.deployments[ret.DeploymentName]
		if age := time.Since(dep.cachedAt); c.ttl >= 0 && age >= c.ttl {
			log.Debugf("Cached deployment (%s) deemed stale. Age: %s, TTL: %s", ret.DeploymentName, age, c.ttl)
			s.invalidateDeployment(ret.DeploymentName)
			ret = nil
		}
	}
	return
}

// Just a helper function that invalidates a single dep.
// Panicks if deployment isn't cached
// SYNC: Expected that you have the lock when you call this function.
func (s *Seeker) invalidateDeployment(name string) {
	log.Debugf("Invalidating cache for deployment (%s)", name)
	c := s.vmcache
	dep, found := c.deployments[name]
	if !found {
		panic(fmt.Sprintf("Tried to delete cache for unknown deployment: %s", name))
	}
	//Delete each ip from the actual cache
	for _, host := range dep.hosts {
		delete(c.data, host)
	}
	//Delete the cache record for this deployment because it's not cached anymore
	delete(c.deployments, name)
}

//This function will cycle through uncached deployments, storing their vm info
// until getting to the relevant deployment with the ip we need.
// It will only search in deployments that aren't already cached.
// Returns the VMInfo if it is found
func (s *Seeker) cacheUntil(ip string) (ret *VMInfo, err error) {
	log.Debugf("Attempting fetch of VM with IP (%s)", ip)

	ip, err = canonizeIP(ip)
	if err != nil {
		return
	}

	for _, dep := range s.config.BOSH.Deployments {
		if s.vmcache.deployments[dep] != nil { //if deployment is cached...
			continue
		}

		var vms []gogobosh.VM
		//Go get the VMs in this particular deployment
		log.Debugf("Contacting BOSH Director for VMs in deployment with name (%s)", dep)
		vms, err = s.bosh.GetDeploymentVMs(dep)
		if err != nil {
			return ret, fmt.Errorf("Error while getting VMs for deployment `%s`: %s", dep, err.Error())
		}

		log.Debugf("Inserting VMs into local memory cache")

		vmsInDeployment := []string{}

		s.acquireLock()
		//Populate the cache with the VMs we got
		for _, vm := range vms {
			//Cache every ip address for this VM as this VM
			for idx, ip := range vm.IPs {
				vm.IPs[idx], err = canonizeIP(ip)
				if err != nil {
					s.releaseLock()
					return
				}
				vmsInDeployment = append(vmsInDeployment, ip)
				s.vmcache.data[ip] = &VMInfo{
					JobName:        vm.JobName,
					DeploymentName: dep,
					IP:             ip,
					Index:          vm.Index,
				}
			}
		}
		log.Debugf("Cached %d VMs", len(vms))

		//Mark that we cached this deployment
		s.vmcache.deployments[dep] = &deploymentEntry{
			hosts:    vmsInDeployment,
			cachedAt: time.Now(),
		}

		//Bail out if we got our target ip
		if vm, found := s.vmcache.data[ip]; found {
			log.Debugf("Found target IP (%s) in deployment (%s)", ip, dep)
			s.releaseLock()
			return vm, nil
		}
		s.releaseLock()
	}
	log.Debugf("Fetched all deployments but didn't find IP (%s)", ip)
	return
}

//InvalidateAll wipes the entire cache
func (s *Seeker) InvalidateAll() {
	log.Debugf("Invalidating cache for Seeker (%p)", s)
	s.acquireLock()
	defer s.releaseLock()
	s.vmcache.data = map[string]*VMInfo{}
	s.vmcache.deployments = map[string]*deploymentEntry{}
	log.Debugf("Cache invalidated for Seeker (%p)", s)
}

func (s *Seeker) acquireLock() {
	log.Debugf("Acquiring lock for Seeker (%p)...", s)
	s.vmcache.lock.Lock()
	log.Debugf("Lock acquired (%p)")
}

func (s *Seeker) releaseLock() {
	s.vmcache.lock.Unlock()
	log.Debugf("Released lock for Seeker (%p)", s)
}
