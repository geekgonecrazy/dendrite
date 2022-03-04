// Copyright 2022 The Matrix.org Foundation C.I.C.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package routing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"

	appserviceAPI "github.com/matrix-org/dendrite/appservice/api"
	"github.com/matrix-org/dendrite/clientapi/jsonerror"
	"github.com/matrix-org/dendrite/roomserver/api"
	"github.com/matrix-org/dendrite/setup/config"
	userapi "github.com/matrix-org/dendrite/userapi/api"
	userdb "github.com/matrix-org/dendrite/userapi/storage"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/util"
	"github.com/sirupsen/logrus"
)

// The data used to populate the /consent request
type constentTemplateData struct {
	User          string
	Version       string
	UserHMAC      string
	HasConsented  bool
	PublicVersion bool
}

func consent(writer http.ResponseWriter, req *http.Request, userAPI userapi.UserInternalAPI, cfg *config.ClientAPI) *util.JSONResponse {
	consentCfg := cfg.Matrix.UserConsentOptions
	internalError := jsonerror.InternalServerError()

	// The data used to populate the /consent request
	data := constentTemplateData{
		User:     req.FormValue("u"),
		Version:  req.FormValue("v"),
		UserHMAC: req.FormValue("h"),
	}
	switch req.Method {
	case http.MethodGet:
		// display the privacy policy without a form
		data.PublicVersion = data.User == "" || data.UserHMAC == "" || data.Version == ""

		// let's see if the user already consented to the current version
		if !data.PublicVersion {
			res := &userapi.QueryPolicyVersionResponse{}
			localPart, _, err := gomatrixserverlib.SplitID('@', data.User)
			if err != nil {
				logrus.WithError(err).Error("unable to print consent template")
				return &internalError
			}
			if err = userAPI.QueryPolicyVersion(req.Context(), &userapi.QueryPolicyVersionRequest{
				LocalPart: localPart,
			}, res); err != nil {
				logrus.WithError(err).Error("unable to print consent template")
				return &internalError
			}
			data.HasConsented = res.PolicyVersion == consentCfg.Version
		}

		err := consentCfg.Templates.ExecuteTemplate(writer, consentCfg.Version+".gohtml", data)
		if err != nil {
			logrus.WithError(err).Error("unable to print consent template")
			return nil
		}
		return nil
	case http.MethodPost:
		localPart, _, err := gomatrixserverlib.SplitID('@', data.User)
		if err != nil {
			logrus.WithError(err).Error("unable to split username")
			return &internalError
		}

		ok, err := validHMAC(data.User, data.UserHMAC, consentCfg.FormSecret)
		if err != nil || !ok {
			_, err = writer.Write([]byte("invalid HMAC provided"))
			if err != nil {
				return &internalError
			}
			return &internalError
		}
		if err := userAPI.PerformUpdatePolicyVersion(
			req.Context(),
			&userapi.UpdatePolicyVersionRequest{
				PolicyVersion: data.Version,
				LocalPart:     localPart,
			},
			&userapi.UpdatePolicyVersionResponse{},
		); err != nil {
			_, err = writer.Write([]byte("unable to update database"))
			if err != nil {
				logrus.WithError(err).Error("unable to write to database")
			}
			return &internalError
		}
		// display the privacy policy without a form
		data.PublicVersion = false
		data.HasConsented = true

		err = consentCfg.Templates.ExecuteTemplate(writer, consentCfg.Version+".gohtml", data)
		if err != nil {
			logrus.WithError(err).Error("unable to print consent template")
			return &internalError
		}
		return nil
	}
	return &util.JSONResponse{Code: http.StatusOK}
}

func sendServerNoticeForConsent(userAPI userapi.UserInternalAPI, rsAPI api.RoomserverInternalAPI,
	cfgNotices *config.ServerNotices,
	cfgClient *config.ClientAPI,
	senderDevice *userapi.Device,
	accountsDB userdb.Database,
	asAPI appserviceAPI.AppServiceQueryAPI,
) {
	res := &userapi.QueryOutdatedPolicyResponse{}
	if err := userAPI.QueryOutdatedPolicy(context.Background(), &userapi.QueryOutdatedPolicyRequest{
		PolicyVersion: cfgClient.Matrix.UserConsentOptions.Version,
	}, res); err != nil {
		logrus.WithError(err).Error("unable to fetch users with outdated consent policy")
		return
	}

	var (
		consentOpts  = cfgClient.Matrix.UserConsentOptions
		data         = make(map[string]string)
		err          error
		sentMessages int
	)

	if len(res.OutdatedUsers) > 0 {
		logrus.WithField("count", len(res.OutdatedUsers)).Infof("Sending server notice to users who have not yet accepted the policy")
	}

	for _, userID := range res.OutdatedUsers {
		if userID == cfgClient.Matrix.ServerNotices.LocalPart {
			continue
		}
		userID = fmt.Sprintf("@%s:%s", userID, cfgClient.Matrix.ServerName)
		data["ConsentURL"], err = buildConsentURI(cfgClient, userID)
		if err != nil {
			logrus.WithError(err).WithField("userID", userID).Error("unable to construct consentURI")
			continue
		}
		msgBody := &bytes.Buffer{}

		if err = consentOpts.TextTemplates.ExecuteTemplate(msgBody, "serverNoticeTemplate", data); err != nil {
			logrus.WithError(err).WithField("userID", userID).Error("unable to execute serverNoticeTemplate")
			continue
		}

		req := sendServerNoticeRequest{
			UserID: userID,
			Content: struct {
				MsgType string `json:"msgtype,omitempty"`
				Body    string `json:"body,omitempty"`
			}{
				MsgType: consentOpts.ServerNoticeContent.MsgType,
				Body:    msgBody.String(),
			},
		}
		_, err = sendServerNotice(context.Background(), req, rsAPI, cfgNotices, cfgClient, senderDevice, accountsDB, asAPI, userAPI, nil, nil, nil)
		if err != nil {
			logrus.WithError(err).WithField("userID", userID).Error("failed to send server notice for consent to user")
			continue
		}
		sentMessages++
		res := &userapi.UpdatePolicyVersionResponse{}
		if err = userAPI.PerformUpdatePolicyVersion(context.Background(), &userapi.UpdatePolicyVersionRequest{
			PolicyVersion:      consentOpts.Version,
			LocalPart:          userID,
			ServerNoticeUpdate: true,
		}, res); err != nil {
			logrus.WithError(err).WithField("userID", userID).Error("failed to update policy version")
			continue
		}
	}
	if sentMessages > 0 {
		logrus.Infof("Sent messages to %d users", sentMessages)
	}
}

func buildConsentURI(cfgClient *config.ClientAPI, userID string) (string, error) {
	consentOpts := cfgClient.Matrix.UserConsentOptions

	mac := hmac.New(sha256.New, []byte(consentOpts.FormSecret))
	_, err := mac.Write([]byte(userID))
	if err != nil {
		return "", err
	}
	userMAC := mac.Sum(nil)

	return fmt.Sprintf("%s/_matrix/client/consent?u=%s&h=%s&v=%s", cfgClient.Matrix.UserConsentOptions.BaseURL, userID, userMAC, consentOpts.Version), nil
}

func validHMAC(username, userHMAC, secret string) (bool, error) {
	mac := hmac.New(sha256.New, []byte(secret))
	_, err := mac.Write([]byte(username))
	if err != nil {
		return false, err
	}
	expectedMAC := mac.Sum(nil)
	decoded, err := hex.DecodeString(userHMAC)
	if err != nil {
		return false, err
	}
	return hmac.Equal(decoded, expectedMAC), nil
}