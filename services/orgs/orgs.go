package orgs

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	api_proto "www.velocidex.com/golang/velociraptor/api/proto"
	config_proto "www.velocidex.com/golang/velociraptor/config/proto"
	"www.velocidex.com/golang/velociraptor/datastore"
	"www.velocidex.com/golang/velociraptor/logging"
	"www.velocidex.com/golang/velociraptor/paths"
	"www.velocidex.com/golang/velociraptor/services"
)

type OrgContext struct {
	record     *api_proto.OrgRecord
	config_obj *config_proto.Config
	service    services.ServiceContainer
}

type OrgManager struct {
	mu sync.Mutex

	ctx context.Context
	wg  *sync.WaitGroup

	// The base global config object
	config_obj *config_proto.Config

	// Each org has a separate config object.
	orgs            map[string]*OrgContext
	org_id_by_nonce map[string]string
}

func (self *OrgManager) ListOrgs() []*api_proto.OrgRecord {
	result := []*api_proto.OrgRecord{}
	self.mu.Lock()
	defer self.mu.Unlock()

	for _, item := range self.orgs {
		result = append(result, proto.Clone(item.record).(*api_proto.OrgRecord))
	}

	// Sort orgs by names
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result
}

func (self *OrgManager) GetOrgConfig(org_id string) (*config_proto.Config, error) {
	self.mu.Lock()
	defer self.mu.Unlock()

	if org_id == "root" {
		org_id = ""
	}

	// An empty org id corresponds to the root org.
	if org_id == "" {
		return self.config_obj, nil
	}

	result, pres := self.orgs[org_id]
	if !pres {
		return nil, services.NotFoundError
	}
	return result.config_obj, nil
}

func (self *OrgManager) GetOrg(org_id string) (*api_proto.OrgRecord, error) {
	self.mu.Lock()
	defer self.mu.Unlock()

	if org_id == "root" {
		org_id = ""
	}

	result, pres := self.orgs[org_id]
	if !pres {
		return nil, services.NotFoundError
	}
	return result.record, nil
}

func (self *OrgManager) OrgIdByNonce(nonce string) (string, error) {
	self.mu.Lock()
	defer self.mu.Unlock()

	// Nonce corresponds to the root config
	if self.config_obj.Client != nil &&
		self.config_obj.Client.Nonce == nonce {
		return "", nil
	}

	result, pres := self.org_id_by_nonce[nonce]
	if !pres {
		return "", services.NotFoundError
	}
	return result, nil
}

func (self *OrgManager) CreateNewOrg(name, id string) (
	*api_proto.OrgRecord, error) {

	if id == "" {
		id = NewOrgId()
	}

	org_record := &api_proto.OrgRecord{
		Name:  name,
		OrgId: id,
		Nonce: NewNonce(),
	}

	// Check if the org already exists
	self.mu.Lock()
	_, pres := self.orgs[id]
	self.mu.Unlock()
	if pres {
		return nil, errors.New("Org ID already exists")
	}

	err := self.startOrg(org_record)
	if err != nil {
		return nil, err
	}

	org_path_manager := paths.NewOrgPathManager(
		org_record.OrgId)
	db, err := datastore.GetDB(self.config_obj)
	if err != nil {
		return nil, err
	}

	err = db.SetSubject(self.config_obj,
		org_path_manager.Path(), org_record)
	return org_record, err
}

func (self *OrgManager) makeNewConfigObj(
	record *api_proto.OrgRecord) *config_proto.Config {

	result := proto.Clone(self.config_obj).(*config_proto.Config)

	result.OrgId = record.OrgId
	result.OrgName = record.Name

	if result.Client != nil {
		// Client config does not leak org id! We use the nonce to tie
		// org id back to the org.
		result.Client.Nonce = record.Nonce
	}

	if result.Datastore != nil && record.OrgId != "" {
		if result.Datastore.Location != "" {
			result.Datastore.Location = filepath.Join(
				result.Datastore.Location, "orgs", record.OrgId)
		}
		if result.Datastore.FilestoreDirectory != "" {
			result.Datastore.FilestoreDirectory = filepath.Join(
				result.Datastore.FilestoreDirectory, "orgs", record.OrgId)
		}
	}

	return result
}

func (self *OrgManager) Scan() error {
	db, err := datastore.GetDB(self.config_obj)
	if err != nil {
		return nil
	}

	children, err := db.ListChildren(
		self.config_obj, paths.ORGS_ROOT)
	if err != nil {
		return err
	}

	for _, org_path := range children {
		org_id := org_path.Base()
		org_path_manager := paths.NewOrgPathManager(org_id)
		org_record := &api_proto.OrgRecord{}
		err := db.GetSubject(self.config_obj,
			org_path_manager.Path(), org_record)
		if err != nil {
			continue
		}

		_, err = self.GetOrgConfig(org_id)
		if err != nil {
			err = self.startOrg(org_record)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (self *OrgManager) Start(
	ctx context.Context,
	config_obj *config_proto.Config,
	wg *sync.WaitGroup) error {
	logger := logging.GetLogger(config_obj, &logging.FrontendComponent)
	logger.Info("<green>Starting</> Org Manager service.")

	nonce := ""
	if config_obj.Client != nil {
		nonce = config_obj.Client.Nonce
	}

	// First start all services for the root org
	err := self.startOrg(&api_proto.OrgRecord{
		OrgId: "",
		Name:  "<root org>",
		Nonce: nonce,
	})
	if err != nil {
		return err
	}

	// If a datastore is not configured we are running on the client
	// or as a tool so we dont need to scan for new orgs.
	if config_obj.Datastore == nil {
		return nil
	}

	// Do first scan inline so we have valid data on exit.
	err = self.Scan()
	if err != nil {
		return err
	}

	// Start syncing the mutation_manager
	wg.Add(1)
	go func() {
		defer wg.Done()

		for {
			select {
			case <-ctx.Done():
				return

			case <-time.After(10 * time.Second):
				self.Scan()
			}
		}

	}()

	return nil
}

func NewOrgManager(
	ctx context.Context,
	wg *sync.WaitGroup,
	config_obj *config_proto.Config) (services.OrgManager, error) {

	service := &OrgManager{
		config_obj: config_obj,
		ctx:        ctx,
		wg:         wg,

		orgs:            make(map[string]*OrgContext),
		org_id_by_nonce: make(map[string]string),
	}

	// Usually only one org manager exists at one time. However for
	// the "gui" command this may be invoked multiple times.
	_, err := services.GetOrgManager()
	if err != nil {
		services.RegisterOrgManager(service)
	}

	return service, service.Start(ctx, config_obj, wg)
}