package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"

	"github.com/gorilla/mux"
	yaml "gopkg.in/yaml.v2"

	"github.com/ovh/cds/engine/api/application"
	"github.com/ovh/cds/engine/api/event"
	"github.com/ovh/cds/engine/api/project"
	"github.com/ovh/cds/engine/service"
	"github.com/ovh/cds/sdk"
	"github.com/ovh/cds/sdk/exportentities"
	"github.com/ovh/cds/sdk/log"
)

func (api *API) postApplicationImportHandler() service.Handler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		// Get project name in URL
		vars := mux.Vars(r)
		key := vars["permProjectKey"]
		force := FormBool(r, "force")

		//Load project
		proj, errp := project.Load(api.mustDB(), api.Cache, key, getUser(ctx), project.LoadOptions.WithGroups, project.LoadOptions.WithPlatforms)
		if errp != nil {
			return sdk.WrapError(errp, "postApplicationImportHandler>> Unable load project")
		}

		body, errr := ioutil.ReadAll(r.Body)
		if errr != nil {
			return sdk.NewError(sdk.ErrWrongRequest, errr)
		}
		defer r.Body.Close()

		contentType := r.Header.Get("Content-Type")
		if contentType == "" {
			contentType = http.DetectContentType(body)
		}

		var eapp = new(exportentities.Application)
		var errapp error
		switch contentType {
		case "application/json":
			errapp = json.Unmarshal(body, eapp)
		case "application/x-yaml", "text/x-yam":
			errapp = yaml.Unmarshal(body, eapp)
		default:
			return sdk.NewError(sdk.ErrWrongRequest, fmt.Errorf("unsupported content-type: %s", contentType))
		}

		if errapp != nil {
			return sdk.NewError(sdk.ErrWrongRequest, errapp)
		}

		tx, err := api.mustDB().Begin()
		if err != nil {
			return sdk.WrapError(err, "postApplicationImportHandler> Unable to start tx")
		}
		defer tx.Rollback()

		newApp, msgList, globalError := application.ParseAndImport(tx, api.Cache, proj, eapp, force, project.DecryptWithBuiltinKey, getUser(ctx))
		msgListString := translate(r, msgList)

		if globalError != nil {
			myError, ok := globalError.(sdk.Error)
			if ok {
				log.Warning("postApplicationImportHandler> Unable to import application %s : %v", eapp.Name, myError)
				msgTranslated, _ := sdk.ProcessError(myError, r.Header.Get("Accept-Language"))
				msgListString = append(msgListString, msgTranslated)
				return service.WriteJSON(w, msgListString, myError.Status)
			}
			return sdk.WrapError(globalError, "postApplicationImportHandler> Unable import application %s", eapp.Name)
		}

		if err := tx.Commit(); err != nil {
			return sdk.WrapError(err, "postApplicationImportHandler> Cannot commit transaction")
		}
		event.PublishAddApplication(proj.Key, *newApp, getUser(ctx))

		return service.WriteJSON(w, msgListString, http.StatusOK)
	}
}
