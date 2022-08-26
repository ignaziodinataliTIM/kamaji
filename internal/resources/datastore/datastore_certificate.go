// Copyright 2022 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package datastore

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kamajiv1alpha1 "github.com/clastix/kamaji/api/v1alpha1"
	"github.com/clastix/kamaji/internal/etcd"
	"github.com/clastix/kamaji/internal/utilities"
)

type Certificate struct {
	resource  *corev1.Secret
	Client    client.Client
	Name      string
	DataStore kamajiv1alpha1.DataStore
}

func (r *Certificate) ShouldStatusBeUpdated(_ context.Context, tenantControlPlane *kamajiv1alpha1.TenantControlPlane) bool {
	return tenantControlPlane.Status.Storage.Certificate.Checksum != r.resource.GetAnnotations()["checksum"]
}

func (r *Certificate) ShouldCleanup(*kamajiv1alpha1.TenantControlPlane) bool {
	return false
}

func (r *Certificate) CleanUp(context.Context, *kamajiv1alpha1.TenantControlPlane) (bool, error) {
	return false, nil
}

func (r *Certificate) Define(_ context.Context, tenantControlPlane *kamajiv1alpha1.TenantControlPlane) error {
	r.resource = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.getPrefixedName(tenantControlPlane),
			Namespace: tenantControlPlane.GetNamespace(),
		},
		Data: map[string][]byte{},
	}

	return nil
}

func (r *Certificate) getPrefixedName(tenantControlPlane *kamajiv1alpha1.TenantControlPlane) string {
	return utilities.AddTenantPrefix(r.GetName(), tenantControlPlane)
}

func (r *Certificate) GetClient() client.Client {
	return r.Client
}

func (r *Certificate) CreateOrUpdate(ctx context.Context, tenantControlPlane *kamajiv1alpha1.TenantControlPlane) (controllerutil.OperationResult, error) {
	return utilities.CreateOrUpdateWithConflict(ctx, r.Client, r.resource, r.mutate(ctx, tenantControlPlane))
}

func (r *Certificate) GetName() string {
	return "datastore-certificate"
}

func (r *Certificate) UpdateTenantControlPlaneStatus(_ context.Context, tenantControlPlane *kamajiv1alpha1.TenantControlPlane) error {
	tenantControlPlane.Status.Storage.Certificate.SecretName = r.resource.GetName()
	tenantControlPlane.Status.Storage.Certificate.Checksum = r.resource.GetAnnotations()["checksum"]
	tenantControlPlane.Status.Storage.Certificate.LastUpdate = metav1.Now()

	return nil
}

func (r *Certificate) mutate(ctx context.Context, tenantControlPlane *kamajiv1alpha1.TenantControlPlane) controllerutil.MutateFn {
	return func() error {
		ca, err := r.DataStore.Spec.TLSConfig.CertificateAuthority.Certificate.GetContent(ctx, r.Client)
		if err != nil {
			return err
		}

		r.resource.Data["ca.crt"] = ca

		if r.resource.GetAnnotations()["checksum"] == utilities.CalculateConfigMapChecksum(r.resource.StringData) {
			if r.DataStore.Spec.Driver == kamajiv1alpha1.EtcdDriver {
				if isValid, _ := etcd.IsETCDCertificateAndKeyPairValid(r.resource.Data["server.crt"], r.resource.Data["server.key"]); isValid {
					return nil
				}
			}
		}

		var crt, key *bytes.Buffer

		switch r.DataStore.Spec.Driver {
		case kamajiv1alpha1.EtcdDriver:
			// When dealing with the etcd storage we cannot use the basic authentication, thus the generation of a
			// certificate used for authentication is mandatory, along with the CA private key.
			privateKey, err := r.DataStore.Spec.TLSConfig.CertificateAuthority.PrivateKey.GetContent(ctx, r.Client)
			if err != nil {
				return err
			}

			crt, key, err = etcd.GetETCDCACertificateAndKeyPair(tenantControlPlane.GetName(), ca, privateKey)
			if err != nil {
				return err
			}
		case kamajiv1alpha1.KineMySQLDriver, kamajiv1alpha1.KinePostgreSQLDriver:
			// For the SQL drivers we just need to copy the certificate, since the basic authentication is used
			// to connect to the desired schema and database.
			crtBytes, err := r.DataStore.Spec.TLSConfig.ClientCertificate.Certificate.GetContent(ctx, r.Client)
			if err != nil {
				return err
			}
			crt = bytes.NewBuffer(crtBytes)

			keyBytes, err := r.DataStore.Spec.TLSConfig.ClientCertificate.PrivateKey.GetContent(ctx, r.Client)
			if err != nil {
				return err
			}
			key = bytes.NewBuffer(keyBytes)
		default:
			return fmt.Errorf("unrecognized driver for Certificate generation")
		}

		r.resource.Data["server.crt"] = crt.Bytes()
		r.resource.Data["server.key"] = key.Bytes()

		annotations := r.resource.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations["checksum"] = utilities.CalculateConfigMapChecksum(r.resource.StringData)
		r.resource.SetAnnotations(annotations)

		r.resource.SetLabels(utilities.MergeMaps(
			utilities.KamajiLabels(),
			r.resource.GetLabels(),
			map[string]string{
				"kamaji.clastix.io/name":      tenantControlPlane.GetName(),
				"kamaji.clastix.io/component": r.GetName(),
			},
		))

		return ctrl.SetControllerReference(tenantControlPlane, r.resource, r.Client.Scheme())
	}
}