/*
Copyright 2020 The Nocalhost Authors.
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

package service_account

import (
	"context"
	"github.com/gin-gonic/gin"
	"nocalhost/internal/nocalhost-api/model"
	"nocalhost/internal/nocalhost-api/service"
	"nocalhost/pkg/nocalhost-api/app/api"
	"nocalhost/pkg/nocalhost-api/app/router/ginbase"
	"nocalhost/pkg/nocalhost-api/pkg/clientgo"
	"nocalhost/pkg/nocalhost-api/pkg/errno"
	"nocalhost/pkg/nocalhost-api/pkg/log"
	"nocalhost/pkg/nocalhost-api/pkg/setupcluster"
	"sync"
)

type SaAuthorizeRequest struct {
	ClusterId *uint64 `json:"cluster_id" binding:"required"`
	UserId    *uint64 `json:"user_id" binding:"required"`
	SpaceName string  `json:"space_name" binding:"required"`
}

func Authorize(c *gin.Context) {
	var req SaAuthorizeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Warnf("bind service account authorizeRequest params err: %v", err)
		api.SendResponse(c, errno.ErrBind, nil)
		return
	}

	err := service.Svc.AuthorizeNsToUser(*req.ClusterId, *req.UserId, req.SpaceName)
	if err != nil {
		api.SendResponse(c, err, nil)
		return
	}

	api.SendResponse(c, nil, nil)
}

func ListAuthorization(c *gin.Context) {
	userId, err := ginbase.LoginUser(c)
	if err != nil {
		api.SendResponse(c, errno.ErrLoginRequired, nil)
		return
	}

	// optimization required
	clusters, err := service.Svc.ClusterSvc().GetList(c)
	if err != nil {
		api.SendResponse(c, errno.ErrClusterNotFound, nil)
		return
	}

	user, err := service.Svc.UserSvc().GetUserByID(c, userId)
	if err != nil {
		api.SendResponse(c, errno.ErrUserNotFound, nil)
		return
	}

	devSpaces, err := service.Svc.ClusterUser().GetList(context.TODO(), model.ClusterUserModel{})
	if err != nil {
		api.SendResponse(c, errno.ErrClusterUserNotFound, nil)
		return
	}

	spaceNameMap := getCluster2Ns2SpaceNameMapping(devSpaces)

	result := []*ServiceAccountModel{}
	var lock sync.Mutex
	wg := sync.WaitGroup{}
	wg.Add(len(clusters))

	for _, cluster := range clusters {
		cluster := cluster
		go func() {
			defer wg.Done()

			// new client go
			clientGo, err := clientgo.NewAdminGoClient([]byte(cluster.KubeConfig))
			if err != nil {
				return
			}

			// nocalhost provide every user a service account each cluster
			var kubeConfig string
			if kubeConfig = getServiceAccountKubeConfig(clientGo, user.SaName, service.NocalhostDefaultSaNs, cluster.Server); kubeConfig == "" {
				return
			}

			privilege := false
			var nss []NS

			// new admin go client will request authorizationv1.SelfSubjectAccessReview
			// then did not find any err, means cluster admin
			if _, err = clientgo.NewAdminGoClient([]byte(kubeConfig)); err == nil {
				privilege = true
			} else {
				for _, ns := range GetAllPermittedNs(string(clientGo.Config), user.SaName) {
					//var spaceName = fmt.Sprintf("Nocalhost-%s", ns)
					//var SpaceId = uint64(0)
					if m, ok := spaceNameMap[cluster.ID]; ok {
						if s, ok := m[ns]; ok {
							nss = append(nss, NS{SpaceName: s.SpaceName, Namespace: ns, SpaceId: s.ID})
						}
					}
				}
			}

			if len(nss) != 0 || privilege {
				lock.Lock()
				result = append(result, &ServiceAccountModel{KubeConfig: kubeConfig, StorageClass: cluster.StorageClass, NS: nss, Privilege: privilege})
				lock.Unlock()
			}
		}()
	}

	wg.Wait()
	api.SendResponse(c, nil, result)
}

func getCluster2Ns2SpaceNameMapping(devSpaces []*model.ClusterUserModel) map[uint64]map[string]*model.ClusterUserModel {
	spaceNameMap := map[uint64]map[string]*model.ClusterUserModel{}
	for _, space := range devSpaces {
		m, ok := spaceNameMap[space.ClusterId]
		if !ok {
			m = map[string]*model.ClusterUserModel{}
		}

		m[space.Namespace] = space
		spaceNameMap[space.ClusterId] = m
	}
	return spaceNameMap
}

func getServiceAccountKubeConfig(clientGo *clientgo.GoClient, saName, saNs, serverAddr string) string {
	sa, err := clientGo.GetServiceAccount(saName, saNs)
	if err != nil || len(sa.Secrets) == 0 {
		return ""
	}

	secret, err := clientGo.GetSecret(sa.Secrets[0].Name, service.NocalhostDefaultSaNs)
	if err != nil {
		return ""
	}

	kubeConfig, _, _ := setupcluster.NewDevKubeConfigReader(secret, serverAddr, saNs).GetCA().GetToken().AssembleDevKubeConfig().ToYamlString()
	return kubeConfig
}

type ServiceAccountModel struct {
	KubeConfig   string `json:"kubeconfig"`
	StorageClass string `json:"storage_class"`
	NS           []NS   `json:"namespace_packs"`
	Privilege    bool   `json:"privilege"`
}

type NS struct {
	SpaceId   uint64 `json:"space_id"`
	Namespace string `json:"namespace"`
	SpaceName string `json:"spacename"`
}