/*
Copyright 2022 The Rook Authors. All rights reserved.

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

package operator

import (
	"context"
	"fmt"
	"time"

	api "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1"
	v1 "github.com/jetstack/cert-manager/pkg/apis/meta/v1"
	cs "github.com/jetstack/cert-manager/pkg/client/clientset/versioned/typed/certmanager/v1"
	"github.com/rook/rook/pkg/clusterd"
	admv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	issuerName        = "selfsigned-issuer"
	certificateName   = "rook-admission-controller-cert"
	webhookConfigName = "rook-ceph-webhook"
)

func fetchorCreateIssuer(ctx context.Context, certMgrClient *cs.CertmanagerV1Client) (*api.Issuer, error) {
	logger.Infof("Fetching Issuer %s/%s.", namespace, issuerName)
	issuer, err := certMgrClient.Issuers(namespace).Get(ctx, issuerName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return createIssuers(ctx, certMgrClient)
		}

		logger.Errorf("failed to get issuer %s. %v", issuerName, err)
		return issuer, err
	}

	return issuer, nil
}

func createIssuers(ctx context.Context, certMgrClient *cs.CertmanagerV1Client) (*api.Issuer, error) {
	logger.Infof("Creating Issuer %s/%s.", namespace, issuerName)
	issuer := &api.Issuer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      issuerName,
			Namespace: namespace,
		},
		Spec: api.IssuerSpec{
			IssuerConfig: api.IssuerConfig{
				SelfSigned: &api.SelfSignedIssuer{},
			},
		},
	}
	issuers, err := certMgrClient.Issuers(namespace).Create(ctx, issuer, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create issuer %s. %v", issuerName, err)
	}

	sleepTime := 1
	attempts := 10

	for i := 0; i < attempts; i++ {
		issuers, err = certMgrClient.Issuers(namespace).Get(ctx, issuerName, metav1.GetOptions{})
		if err != nil {
			logger.Errorf("failed to get issuer %s. %v", issuerName, err)
			return issuers, err
		}

		if len(issuers.Status.Conditions) != 0 {
			if issuers.Status.Conditions[0].Reason == "IsReady" && issuers.Status.Conditions[0].Status == "True" {
				return issuers, nil
			}
			logger.Debugf("webhook config %q status=%+v", issuerName, issuers.Status.Conditions[0].Status)
		}

		time.Sleep(time.Duration(sleepTime) * time.Second)
		logger.Infof("waiting for webhook config %q to be in ready status", issuerName)
	}
	return issuers, nil
}

func fetchorCreateCertificate(ctx context.Context, certMgrClient *cs.CertmanagerV1Client, issuer *api.Issuer) error {
	logger.Infof("Fetching certificate %s/%s.", namespace, certificateName)
	_, err := certMgrClient.Certificates(namespace).Get(ctx, certificateName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return createCertificate(ctx, certMgrClient, issuer)
		}
		logger.Errorf("failed to get certificate %s. %v", certificateName, err)
	}

	return nil
}

func createCertificate(ctx context.Context, certMgrClient *cs.CertmanagerV1Client, issuer *api.Issuer) error {
	logger.Infof("Creating certificate %s/%s.", namespace, certificateName)
	certificate := &api.Certificate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      certificateName,
			Namespace: namespace,
		},
		Spec: api.CertificateSpec{
			DNSNames: []string{
				admissionControllerAppName,
				fmt.Sprintf("%s.%s.svc", admissionControllerAppName, namespace),
				fmt.Sprintf("%s.%s.svc.cluster.local", admissionControllerAppName, namespace)},
			IssuerRef: v1.ObjectReference{
				Name: issuer.Name,
				Kind: "Issuer",
			},
			SecretName: admissionControllerAppName,
		},
	}

	_, err := certMgrClient.Certificates(namespace).Create(ctx, certificate, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create certificate %s. %v", certificateName, err)
	}
	sleepTime := 1
	attempts := 10

	for i := 0; i < attempts; i++ {
		cert, err := certMgrClient.Certificates(namespace).Get(ctx, certificateName, metav1.GetOptions{})
		if err != nil {
			logger.Errorf("failed to get certificate %s. %v", certificateName, err)
		}
		if len(cert.Status.Conditions) != 0 {
			if cert.Status.Conditions[0].Reason == "Ready" && cert.Status.Conditions[0].Status == "True" {
				return nil
			}
			logger.Debugf("webhook config %q status=%+v", certificateName, cert.Status.Conditions[0].Status)
		}

		logger.Infof("waiting for webhook config %q to be in ready status", certificateName)
		time.Sleep(time.Duration(sleepTime) * time.Second)
	}
	return nil
}

func fetchValidatingWebhookConfig(ctx context.Context, clusterdContext *clusterd.Context) error {

	logger.Infof("Fetching webhook %s/%s.", namespace, webhookConfigName)
	_, err := clusterdContext.Clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, webhookConfigName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return createValidatingWebhookConfig(ctx, clusterdContext)
		}
		logger.Errorf("failed to get webhook %s. %v", webhookConfigName, err)
	}

	return nil
}

func createValidatingWebhookConfig(ctx context.Context, clusterdContext *clusterd.Context) error {
	sideEffects := admv1.SideEffectClassNone
	var timeout int32 = 5
	serviceCephClusterPath := "/validate-ceph-rook-io-v1-cephcluster"
	serviceCephBlockPoolPath := "/validate-ceph-rook-io-v1-cephblockpool"
	serviceCephObjectStorePath := "/validate-ceph-rook-io-v1-cephobjectstore"

	logger.Infof("Creating webhook %s/%s.", namespace, webhookConfigName)

	validatingWebhook := &admv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      webhookConfigName,
			Namespace: namespace,
			Annotations: map[string]string{
				"cert-manager.io/inject-ca-from": fmt.Sprintf("%s/rook-admission-controller-cert", namespace),
			},
		},
		Webhooks: []admv1.ValidatingWebhook{
			{
				Name: fmt.Sprintf("cephcluster-wh-%s-%s.rook.io", admissionControllerAppName, namespace),
				Rules: []admv1.RuleWithOperations{
					{
						Rule: admv1.Rule{
							APIGroups:   []string{"ceph.rook.io"},
							APIVersions: []string{"v1"},
							Resources:   []string{"cephclusters"},
						},
						Operations: []admv1.OperationType{
							admv1.Update,
							admv1.Create,
							admv1.Delete,
						},
					},
				},
				ClientConfig: admv1.WebhookClientConfig{
					Service: &admv1.ServiceReference{
						Name:      admissionControllerAppName,
						Namespace: namespace,
						Path:      &serviceCephClusterPath,
					},
				},
				AdmissionReviewVersions: []string{"v1", "v1beta1"},
				SideEffects:             &sideEffects,
				TimeoutSeconds:          &timeout,
			},
			{
				Name: fmt.Sprintf("cephblockpool-wh-%s-%s.rook.io", admissionControllerAppName, namespace),
				Rules: []admv1.RuleWithOperations{
					{
						Rule: admv1.Rule{
							APIGroups:   []string{"ceph.rook.io"},
							APIVersions: []string{"v1"},
							Resources:   []string{"cephblockpools"},
						},
						Operations: []admv1.OperationType{
							admv1.Update,
							admv1.Create,
							admv1.Delete,
						},
					},
				},
				ClientConfig: admv1.WebhookClientConfig{
					Service: &admv1.ServiceReference{
						Name:      admissionControllerAppName,
						Namespace: namespace,
						Path:      &serviceCephBlockPoolPath,
					},
				},
				AdmissionReviewVersions: []string{"v1", "v1beta1"},
				SideEffects:             &sideEffects,
				TimeoutSeconds:          &timeout,
			},
			{
				Name: fmt.Sprintf("cephobjectstore-wh-%s-%s.rook.io", admissionControllerAppName, namespace),
				Rules: []admv1.RuleWithOperations{
					{
						Rule: admv1.Rule{
							APIGroups:   []string{"ceph.rook.io"},
							APIVersions: []string{"v1"},
							Resources:   []string{"cephobjectstores"},
						},
						Operations: []admv1.OperationType{
							admv1.Update,
							admv1.Create,
							admv1.Delete,
						},
					},
				},
				ClientConfig: admv1.WebhookClientConfig{
					Service: &admv1.ServiceReference{
						Name:      admissionControllerAppName,
						Namespace: namespace,
						Path:      &serviceCephObjectStorePath,
					},
				},
				AdmissionReviewVersions: []string{"v1", "v1beta1"},
				SideEffects:             &sideEffects,
				TimeoutSeconds:          &timeout,
			},
		},
	}

	_, err := clusterdContext.Clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(ctx, validatingWebhook, metav1.CreateOptions{})
	if err != nil {
		logger.Errorf("failed to create validating webhook %s. %v", webhookConfigName, err)
	}

	return nil
}

func deleteIssuerAndCetificate(ctx context.Context, certMgrClient *cs.CertmanagerV1Client, clusterdContext *clusterd.Context) error {

	err := clusterdContext.Clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations().Delete(ctx, webhookConfigName, metav1.DeleteOptions{})
	if err != nil && !kerrors.IsNotFound(err) {
		logger.Errorf("failed to delete validating webhook %s. %v", webhookConfigName, err)
		return err
	}
	err = certMgrClient.Certificates(namespace).Delete(ctx, certificateName, metav1.DeleteOptions{})
	if err != nil && !kerrors.IsNotFound(err) {
		logger.Errorf("failed to delete issuer %s. %v", issuerName, err)
		return err
	}

	err = certMgrClient.Issuers(namespace).Delete(ctx, issuerName, metav1.DeleteOptions{})
	if err != nil && !kerrors.IsNotFound(err) {
		logger.Errorf("failed to delete certificate %s. %v", certificateName, err)
		return err
	}
	return nil
}
