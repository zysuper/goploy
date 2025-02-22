// Copyright 2022 The Goploy Authors. All rights reserved.
// Use of this source code is governed by a GPLv3-style
// license that can be found in the LICENSE file.

package api

import (
	"bytes"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/pkg/sftp"
	"github.com/zhenorzz/goploy/cmd/server/api/middleware"
	"github.com/zhenorzz/goploy/cmd/server/task"
	"github.com/zhenorzz/goploy/config"
	"github.com/zhenorzz/goploy/internal/log"
	"github.com/zhenorzz/goploy/internal/model"
	"github.com/zhenorzz/goploy/internal/pkg"
	"github.com/zhenorzz/goploy/internal/pkg/cmd"
	"github.com/zhenorzz/goploy/internal/repo"
	"github.com/zhenorzz/goploy/internal/server"
	"github.com/zhenorzz/goploy/internal/server/response"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

type Deploy API

func (d Deploy) Handler() []server.Route {
	return []server.Route{
		server.NewRoute("/deploy/getList", http.MethodGet, d.GetList).Permissions(config.ShowDeployPage),
		server.NewRoute("/deploy/getPublishTrace", http.MethodGet, d.GetPublishTrace).Permissions(config.DeployDetail),
		server.NewRoute("/deploy/getPublishTraceDetail", http.MethodGet, d.GetPublishTraceDetail).Permissions(config.DeployDetail),
		server.NewRoute("/deploy/getPreview", http.MethodGet, d.GetPreview).Permissions(config.DeployDetail),
		server.NewRoute("/deploy/review", http.MethodPut, d.Review).Permissions(config.DeployReview).LogFunc(middleware.AddOPLog),
		server.NewRoute("/deploy/resetState", http.MethodPut, d.ResetState).Permissions(config.DeployResetState).LogFunc(middleware.AddOPLog),
		server.NewRoute("/deploy/publish", http.MethodPost, d.Publish).Permissions(config.DeployProject).Middleware(middleware.HasProjectPermission).LogFunc(middleware.AddOPLog),
		server.NewRoute("/deploy/rebuild", http.MethodPost, d.Rebuild).Permissions(config.DeployRollback).Middleware(middleware.HasProjectPermission).LogFunc(middleware.AddOPLog),
		server.NewRoute("/deploy/greyPublish", http.MethodPost, d.GreyPublish).Permissions(config.GreyDeploy).Middleware(middleware.HasProjectPermission).LogFunc(middleware.AddOPLog),
		server.NewWhiteRoute("/deploy/webhook", http.MethodPost, d.Webhook).Middleware(middleware.FilterEvent),
		server.NewWhiteRoute("/deploy/callback", http.MethodGet, d.Callback),
		server.NewRoute("/deploy/fileCompare", http.MethodPost, d.FileCompare).Permissions(config.FileCompare),
		server.NewRoute("/deploy/fileDiff", http.MethodPost, d.FileDiff).Permissions(config.FileCompare),
		server.NewRoute("/deploy/manageProcess", http.MethodPost, d.ManageProcess).Permissions(config.ProcessManager).LogFunc(middleware.AddOPLog),
	}
}

func (Deploy) GetList(gp *server.Goploy) server.Response {
	var projects model.Projects
	var err error
	if _, ok := gp.Namespace.PermissionIDs[config.GetAllDeployList]; ok {
		projects, err = model.Project{NamespaceID: gp.Namespace.ID}.GetDeployList()
	} else {
		projects, err = model.Project{NamespaceID: gp.Namespace.ID, UserID: gp.UserInfo.ID}.GetDeployList()
	}
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	return response.JSON{
		Data: struct {
			Project model.Projects `json:"list"`
		}{Project: projects},
	}
}

func (Deploy) GetPreview(gp *server.Goploy) server.Response {
	type ReqData struct {
		ProjectID  int64  `schema:"projectId" validate:"gt=0"`
		UserID     int64  `schema:"userId"`
		State      int    `schema:"state"`
		CommitDate string `schema:"commitDate"`
		DeployDate string `schema:"deployDate"`
		Branch     string `schema:"branch"`
		Commit     string `schema:"commit"`
		Filename   string `schema:"filename"`
	}
	var reqData ReqData
	if err := decodeQuery(gp.URLQuery, &reqData); err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	pagination, err := model.PaginationFrom(gp.URLQuery)
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	commitDate := strings.Split(reqData.CommitDate, ",")
	for i, date := range commitDate {
		tm2, _ := time.Parse("2006-01-02 15:04:05", date)
		commitDate[i] = strconv.FormatInt(tm2.Unix(), 10)
	}
	gitTraceList, pagination, err := model.PublishTrace{
		ProjectID:   reqData.ProjectID,
		PublisherID: reqData.UserID,
		State:       reqData.State,
	}.GetPreview(
		reqData.Branch,
		reqData.Commit,
		reqData.Filename,
		commitDate,
		strings.Split(reqData.DeployDate, ","),
		pagination,
	)
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}
	return response.JSON{
		Data: struct {
			GitTraceList model.PublishTraces `json:"list"`
			Pagination   model.Pagination    `json:"pagination"`
		}{GitTraceList: gitTraceList, Pagination: pagination},
	}
}

func (Deploy) GetPublishTrace(gp *server.Goploy) server.Response {
	lastPublishToken := gp.URLQuery.Get("lastPublishToken")
	publishTraceList, err := model.PublishTrace{Token: lastPublishToken}.GetListByToken()
	if err == sql.ErrNoRows {
		return response.JSON{Code: response.Error, Message: "No deploy record"}
	} else if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}
	return response.JSON{
		Data: struct {
			PublishTraceList model.PublishTraces `json:"list"`
		}{PublishTraceList: publishTraceList},
	}
}

func (Deploy) GetPublishTraceDetail(gp *server.Goploy) server.Response {
	id, err := strconv.ParseInt(gp.URLQuery.Get("id"), 10, 64)
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}
	detail, err := model.PublishTrace{ID: id}.GetDetail()
	if err == sql.ErrNoRows {
		return response.JSON{Code: response.Error, Message: "No deploy record"}
	} else if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}
	return response.JSON{
		Data: struct {
			Detail string `json:"detail"`
		}{Detail: detail},
	}
}

func (Deploy) ResetState(gp *server.Goploy) server.Response {
	type ReqData struct {
		ProjectID int64 `json:"projectId" validate:"gt=0"`
	}
	var reqData ReqData
	if err := decodeJson(gp.Body, &reqData); err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	if err := (model.Project{ID: reqData.ProjectID}).ResetState(); err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	return response.JSON{}
}

func (Deploy) FileCompare(gp *server.Goploy) server.Response {
	type ReqData struct {
		ProjectID int64  `json:"projectId" validate:"gt=0"`
		FilePath  string `json:"filePath" validate:"required"`
	}
	var reqData ReqData
	if err := decodeJson(gp.Body, &reqData); err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	project, err := model.Project{ID: reqData.ProjectID}.GetData()
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	srcPath := path.Join(config.GetProjectPath(reqData.ProjectID), reqData.FilePath)
	file, err := os.Open(srcPath)
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}
	defer file.Close()
	hash := md5.New()
	_, _ = io.Copy(hash, file)
	srcMD5 := hex.EncodeToString(hash.Sum(nil))
	projectServers, err := model.ProjectServer{ProjectID: reqData.ProjectID}.GetBindServerListByProjectID()
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	if len(projectServers) == 0 {
		return response.JSON{Code: response.Error, Message: "project have no projectServer"}
	}

	type FileCompareData struct {
		ServerName string `json:"serverName"`
		ServerIP   string `json:"serverIP"`
		ServerID   int64  `json:"serverId"`
		Status     string `json:"status"`
		IsModified bool   `json:"isModified"`
	}
	var fileCompareList []FileCompareData
	ch := make(chan FileCompareData, len(projectServers))

	distPath := path.Join(project.Path, reqData.FilePath)
	for _, projectServer := range projectServers {
		go func(server model.ProjectServer) {
			fileCompare := FileCompareData{server.ServerName, server.ServerIP, server.ServerID, "no change", false}
			client, err := server.ToSSHConfig().Dial()
			if err != nil {
				fileCompare.Status = "client error"
				ch <- fileCompare
				return
			}
			defer client.Close()

			//此时获取了sshClient，下面使用sshClient构建sftpClient
			sftpClient, err := sftp.NewClient(client)
			if err != nil {
				fileCompare.Status = "sftp error"
				ch <- fileCompare
				return
			}
			defer sftpClient.Close()
			file, err := sftpClient.Open(distPath)
			if err != nil {
				fileCompare.Status = "remote file not exists"
				ch <- fileCompare
				return
			}
			defer file.Close()
			hash := md5.New()
			_, _ = io.Copy(hash, file)
			distMD5 := hex.EncodeToString(hash.Sum(nil))
			if srcMD5 != distMD5 {
				fileCompare.Status = "modified"
				fileCompare.IsModified = true
				ch <- fileCompare
				return
			}
			ch <- fileCompare
		}(projectServer)
	}

	for i := 0; i < len(projectServers); i++ {
		fileCompareList = append(fileCompareList, <-ch)
	}
	close(ch)
	return response.JSON{Data: fileCompareList}
}

func (Deploy) FileDiff(gp *server.Goploy) server.Response {
	type ReqData struct {
		ProjectID int64  `json:"projectId" validate:"gt=0"`
		ServerID  int64  `json:"serverId" validate:"gt=0"`
		FilePath  string `json:"filePath" validate:"required"`
	}
	var reqData ReqData
	if err := decodeJson(gp.Body, &reqData); err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	project, err := model.Project{ID: reqData.ProjectID}.GetData()
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	srcText, err := os.ReadFile(path.Join(config.GetProjectPath(reqData.ProjectID), reqData.FilePath))
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	srv, err := model.Server{ID: reqData.ServerID}.GetData()
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	client, err := srv.ToSSHConfig().Dial()
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}
	defer client.Close()

	//此时获取了sshClient，下面使用sshClient构建sftpClient
	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}
	defer sftpClient.Close()

	distFile, err := sftpClient.Open(path.Join(project.Path, reqData.FilePath))
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}
	defer distFile.Close()
	distText, err := io.ReadAll(distFile)
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}
	return response.JSON{Data: struct {
		SrcText  string `json:"srcText"`
		DistText string `json:"distText"`
	}{SrcText: string(srcText), DistText: string(distText)}}
}

func (Deploy) ManageProcess(gp *server.Goploy) server.Response {
	type ReqData struct {
		ServerID         int64  `json:"serverId" validate:"gt=0"`
		ProjectProcessID int64  `json:"projectProcessId" validate:"gt=0"`
		Command          string `json:"command" validate:"required"`
	}
	var reqData ReqData
	if err := decodeJson(gp.Body, &reqData); err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	projectProcess, err := model.ProjectProcess{ID: reqData.ProjectProcessID}.GetData()
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}
	project, err := (model.Project{ID: projectProcess.ProjectID}).GetData()
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}
	srv, err := (model.Server{ID: reqData.ServerID}).GetData()
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	script := ""
	switch reqData.Command {
	case "status":
		script = projectProcess.Status
	case "start":
		script = projectProcess.Start
	case "stop":
		script = projectProcess.Stop
	case "restart":
		script = projectProcess.Restart
	default:
		return response.JSON{Code: response.Error, Message: "Command error"}
	}
	if script == "" {
		return response.JSON{Code: response.Error, Message: "Command empty"}
	}

	script = project.ReplaceVars(script)

	client, err := srv.ToSSHConfig().Dial()
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}
	defer session.Close()

	var sshOutbuf, sshErrbuf bytes.Buffer
	session.Stdout = &sshOutbuf
	session.Stderr = &sshErrbuf
	err = session.Run(script)
	log.Trace(fmt.Sprintf("%s exec cmd %s, result %t, stdout: %s, stderr: %s", gp.UserInfo.Name, script, err == nil, sshOutbuf.String(), sshErrbuf.String()))
	return response.JSON{
		Data: struct {
			ExecRes bool   `json:"execRes"`
			Stdout  string `json:"stdout"`
			Stderr  string `json:"stderr"`
		}{ExecRes: err == nil, Stdout: sshOutbuf.String(), Stderr: sshErrbuf.String()},
	}
}

func (Deploy) Publish(gp *server.Goploy) server.Response {
	type ReqData struct {
		ProjectID int64  `json:"projectId" validate:"gt=0"`
		Commit    string `json:"commit"`
		Branch    string `json:"branch"`
	}
	var reqData ReqData
	if err := decodeJson(gp.Body, &reqData); err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}
	project, err := model.Project{ID: reqData.ProjectID}.GetData()
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	if project.DeployState == model.ProjectNotDeploy {
		err = projectDeploy(gp, project, "", "")
	} else if project.Review == model.Enable {
		err = projectReview(gp, project, reqData.Commit, reqData.Branch)
	} else {
		err = projectDeploy(gp, project, reqData.Commit, reqData.Branch)
	}
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}
	return response.JSON{}
}

func (Deploy) Rebuild(gp *server.Goploy) server.Response {
	type ReqData struct {
		Token string `json:"token"`
	}
	var reqData ReqData
	if err := decodeJson(gp.Body, &reqData); err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}
	var err error
	publishTraceList, err := model.PublishTrace{Token: reqData.Token}.GetListByToken()
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}
	projectID := publishTraceList[0].ProjectID
	project, err := model.Project{ID: projectID}.GetData()
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	projectServers, err := model.ProjectServer{ProjectID: projectID}.GetBindServerListByProjectID()
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	needToPublish := project.SymlinkPath == ""
	var commitInfo repo.CommitInfo
	publishTraceServerCount := 0
	for _, publishTrace := range publishTraceList {
		// publish failed
		if publishTrace.State == 0 {
			needToPublish = true
			break
		}

		if publishTrace.Type == model.Pull {
			err := json.Unmarshal([]byte(publishTrace.Ext), &commitInfo)
			if err != nil {
				return response.JSON{Code: response.Error, Message: err.Error()}
			}
		} else if publishTrace.Type == model.Deploy {
			for _, projectServer := range projectServers {
				if strings.Contains(publishTrace.Ext, projectServer.ServerIP) {
					publishTraceServerCount++
					break
				}
			}
		}
	}

	// project server has changed
	if publishTraceServerCount != len(projectServers) {
		needToPublish = true
	}
	if needToPublish == false {
		ch := make(chan bool, len(projectServers))
		for _, projectServer := range projectServers {
			go func(projectServer model.ProjectServer) {
				destDir := path.Join(project.SymlinkPath, project.LastPublishToken)
				cmdEntity := cmd.New(projectServer.ServerOS)
				afterDeployCommands := []string{cmdEntity.Symlink(destDir, project.Path), cmdEntity.ChangeDirTime(destDir)}
				if len(project.AfterDeployScript) != 0 {
					scriptName := fmt.Sprintf("goploy-after-deploy-p%d-s%d.%s", project.ID, projectServer.ServerID, pkg.GetScriptExt(project.AfterDeployScriptMode))
					scriptContent := project.ReplaceVars(project.AfterDeployScript)
					scriptContent = projectServer.ReplaceVars(scriptContent)
					err = os.WriteFile(path.Join(config.GetProjectPath(project.ID), scriptName), []byte(project.ReplaceVars(project.AfterDeployScript)), 0755)
					if err != nil {
						log.Error("projectID:" + strconv.FormatInt(project.ID, 10) + " write file err: " + err.Error())
						ch <- false
						return
					}
					afterDeployScriptPath := path.Join(project.Path, scriptName)
					afterDeployCommands = append(afterDeployCommands, cmdEntity.Script(project.AfterDeployScriptMode, afterDeployScriptPath))
					afterDeployCommands = append(afterDeployCommands, cmdEntity.Remove(afterDeployScriptPath))
				}
				client, err := projectServer.ToSSHConfig().Dial()
				if err != nil {
					log.Error("projectID:" + strconv.FormatInt(project.ID, 10) + " dial err: " + err.Error())
					ch <- false
					return
				}
				defer client.Close()
				session, err := client.NewSession()
				if err != nil {
					log.Error("projectID:" + strconv.FormatInt(project.ID, 10) + " new session err: " + err.Error())
					ch <- false
					return
				}

				// check if the path is existed or not
				if output, err := session.CombinedOutput("cd " + destDir); err != nil {
					log.Error("projectID:" + strconv.FormatInt(project.ID, 10) + " check symlink path err: " + err.Error() + ", detail: " + string(output))
					ch <- false
					return
				}
				session, err = client.NewSession()
				if err != nil {
					log.Error("projectID:" + strconv.FormatInt(project.ID, 10) + " new session err: " + err.Error())
					ch <- false
					return
				}

				// redirect to project path
				if output, err := session.CombinedOutput(strings.Join(afterDeployCommands, "&&")); err != nil {
					log.Error("projectID:" + strconv.FormatInt(project.ID, 10) + " symlink err: " + err.Error() + ", detail: " + string(output))
					ch <- false
					return
				}
				ch <- true
			}(projectServer)
		}

		for i := 0; i < len(projectServers); i++ {
			if <-ch == false {
				needToPublish = true
				break
			}
		}
		close(ch)
		if needToPublish == false {
			_ = model.PublishTrace{
				Token:      reqData.Token,
				UpdateTime: time.Now().Format("20060102150405"),
			}.EditUpdateTimeByToken()
			project.PublisherID = gp.UserInfo.ID
			project.PublisherName = gp.UserInfo.Name
			project.LastPublishToken = reqData.Token
			_ = project.Publish()
			return response.JSON{Data: "symlink"}
		}
	}

	if needToPublish == true {
		repoEntity, _ := repo.GetRepo(project.RepoType)
		if !repoEntity.CanRollback() {
			return response.JSON{Code: response.Error, Message: fmt.Sprintf("plesae enable symlink to rollback the %s repo", project.RepoType)}
		}

		project.PublisherID = gp.UserInfo.ID
		project.PublisherName = gp.UserInfo.Name
		project.DeployState = model.ProjectDeploying
		project.LastPublishToken = uuid.New().String()
		err = project.Publish()
		if err != nil {
			return response.JSON{Code: response.Error, Message: err.Error()}
		}
		task.AddDeployTask(task.Gsync{
			UserInfo:       gp.UserInfo,
			Project:        project,
			ProjectServers: projectServers,
			CommitID:       commitInfo.Commit,
			Branch:         commitInfo.Branch,
		})
	}
	return response.JSON{Data: "publish"}
}

func (Deploy) GreyPublish(gp *server.Goploy) server.Response {
	type ReqData struct {
		ProjectID int64   `json:"projectId" validate:"gt=0"`
		Commit    string  `json:"commit"`
		Branch    string  `json:"branch"`
		ServerIDs []int64 `json:"serverIds"`
	}
	var reqData ReqData
	if err := decodeJson(gp.Body, &reqData); err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}
	project, err := model.Project{ID: reqData.ProjectID}.GetData()

	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	bindProjectServers, err := model.ProjectServer{ProjectID: project.ID}.GetBindServerListByProjectID()

	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	projectServers := model.ProjectServers{}

	for _, projectServer := range bindProjectServers {
		for _, serverID := range reqData.ServerIDs {
			if projectServer.ServerID == serverID {
				projectServers = append(projectServers, projectServer)
			}
		}
	}

	project.PublisherID = gp.UserInfo.ID
	project.PublisherName = gp.UserInfo.Name
	project.DeployState = model.ProjectDeploying
	project.LastPublishToken = uuid.New().String()
	err = project.Publish()
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}
	task.AddDeployTask(task.Gsync{
		UserInfo:       gp.UserInfo,
		Project:        project,
		ProjectServers: projectServers,
		CommitID:       reqData.Commit,
		Branch:         reqData.Branch,
	})

	return response.JSON{}
}

func (Deploy) Review(gp *server.Goploy) server.Response {
	type ReqData struct {
		ProjectReviewID int64 `json:"projectReviewId" validate:"gt=0"`
		State           uint8 `json:"state" validate:"gt=0"`
	}

	var reqData ReqData
	if err := decodeJson(gp.Body, &reqData); err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	projectReviewModel := model.ProjectReview{
		ID:       reqData.ProjectReviewID,
		State:    reqData.State,
		Editor:   gp.UserInfo.Name,
		EditorID: gp.UserInfo.ID,
	}
	projectReview, err := projectReviewModel.GetData()
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	if projectReview.State != model.PENDING {
		return response.JSON{Code: response.Error, Message: "Project review state is invalid"}
	}

	if reqData.State == model.APPROVE {
		project, err := model.Project{ID: projectReview.ProjectID}.GetData()
		if err != nil {
			return response.JSON{Code: response.Error, Message: err.Error()}
		}
		if err := projectDeploy(gp, project, projectReview.CommitID, projectReview.Branch); err != nil {
			return response.JSON{Code: response.Error, Message: err.Error()}
		}
	}

	if err = projectReviewModel.EditRow(); err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	return response.JSON{}
}

func (Deploy) Webhook(gp *server.Goploy) server.Response {
	projectID, err := strconv.ParseInt(gp.URLQuery.Get("project_id"), 10, 64)
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}
	type ReqData struct {
		Ref string `json:"ref" validate:"required"`
	}
	var reqData ReqData
	if err := decodeJson(gp.Body, &reqData); err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	project, err := model.Project{ID: projectID}.GetData()
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	if project.State != model.Disable {
		return response.JSON{Code: response.Deny, Message: "Project is disabled"}
	}

	if project.AutoDeploy != model.ProjectWebhookDeploy {
		return response.JSON{Code: response.Deny, Message: "Webhook auto deploy turn off, go to project setting turn on"}
	}

	branch := ""
	if project.RepoType == model.RepoSVN {
		branch = reqData.Ref
	} else {
		branch = strings.Split(reqData.Ref, "/")[2]
	}

	if project.Branch != branch {
		return response.JSON{Code: response.Deny, Message: "Receive branch:" + branch + " push event, not equal to current branch"}
	}

	gp.UserInfo, err = model.User{ID: 1}.GetData()
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	projectServers, err := model.ProjectServer{ProjectID: project.ID}.GetBindServerListByProjectID()
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}
	project.PublisherID = gp.UserInfo.ID
	project.PublisherName = "webhook"
	project.DeployState = model.ProjectDeploying
	project.LastPublishToken = uuid.New().String()
	err = project.Publish()
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}
	task.AddDeployTask(task.Gsync{
		UserInfo:       gp.UserInfo,
		Project:        project,
		ProjectServers: projectServers,
	})
	return response.JSON{Message: "receive push signal"}
}

func (Deploy) Callback(gp *server.Goploy) server.Response {
	projectReviewID, err := strconv.ParseInt(gp.URLQuery.Get("project_review_id"), 10, 64)
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	projectReviewModel := model.ProjectReview{
		ID:       projectReviewID,
		State:    model.APPROVE,
		Editor:   "admin",
		EditorID: 1,
	}
	projectReview, err := projectReviewModel.GetData()
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	if projectReview.State != model.PENDING {
		return response.JSON{Code: response.Error, Message: "Project review state is invalid"}
	}

	project, err := model.Project{ID: projectReview.ProjectID}.GetData()
	if err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}
	if err := projectDeploy(gp, project, projectReview.CommitID, projectReview.Branch); err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	if err = projectReviewModel.EditRow(); err != nil {
		return response.JSON{Code: response.Error, Message: err.Error()}
	}

	return response.JSON{}
}

func projectDeploy(gp *server.Goploy, project model.Project, commitID string, branch string) error {
	projectServers, err := model.ProjectServer{ProjectID: project.ID}.GetBindServerListByProjectID()
	if err != nil {
		return err
	}
	project.PublisherID = gp.UserInfo.ID
	project.PublisherName = gp.UserInfo.Name
	project.DeployState = model.ProjectDeploying
	project.LastPublishToken = uuid.New().String()
	err = project.Publish()
	if err != nil {
		return err
	}
	task.AddDeployTask(task.Gsync{
		UserInfo:       gp.UserInfo,
		Project:        project,
		ProjectServers: projectServers,
		CommitID:       commitID,
		Branch:         branch,
	})
	return nil
}

func projectReview(gp *server.Goploy, project model.Project, commitID string, branch string) error {
	if len(commitID) == 0 {
		return errors.New("commit id is required")
	}
	projectReviewModel := model.ProjectReview{
		ProjectID: project.ID,
		Branch:    branch,
		CommitID:  commitID,
		Creator:   gp.UserInfo.Name,
		CreatorID: gp.UserInfo.ID,
	}
	reviewURL := project.ReviewURL
	if len(reviewURL) > 0 {
		reviewURL = strings.Replace(reviewURL, "__PROJECT_ID__", strconv.FormatInt(project.ID, 10), 1)
		reviewURL = strings.Replace(reviewURL, "__PROJECT_NAME__", project.Name, 1)
		reviewURL = strings.Replace(reviewURL, "__BRANCH__", branch, 1)
		reviewURL = strings.Replace(reviewURL, "__ENVIRONMENT__", strconv.Itoa(int(project.Environment)), 1)
		reviewURL = strings.Replace(reviewURL, "__COMMIT_ID__", commitID, 1)
		reviewURL = strings.Replace(reviewURL, "__PUBLISH_TIME__", strconv.FormatInt(time.Now().Unix(), 10), 1)
		reviewURL = strings.Replace(reviewURL, "__PUBLISHER_ID__", gp.UserInfo.Name, 1)
		reviewURL = strings.Replace(reviewURL, "__PUBLISHER_NAME__", strconv.FormatInt(gp.UserInfo.ID, 10), 1)

		projectReviewModel.ReviewURL = reviewURL
	}
	id, err := projectReviewModel.AddRow()
	if err != nil {
		return err
	}
	if len(reviewURL) > 0 {
		callback := "http://"
		if gp.Request.TLS != nil {
			callback = "https://"
		}
		callback += gp.Request.Host + "/deploy/callback?project_review_id=" + strconv.FormatInt(id, 10)
		callback = url.QueryEscape(callback)
		reviewURL = strings.Replace(reviewURL, "__CALLBACK__", callback, 1)

		resp, err := http.Get(reviewURL)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
	}
	return nil
}
