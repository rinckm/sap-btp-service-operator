/*


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

package controllers

import (
	"context"
	"encoding/json"
	"time"

	"fmt"

	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	servicesv1 "github.com/SAP/sap-btp-service-operator/api/v1"
	"github.com/SAP/sap-btp-service-operator/internal/secrets/template"
	"github.com/pkg/errors"

	"github.com/SAP/sap-btp-service-operator/api"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/google/uuid"

	"github.com/SAP/sap-btp-service-operator/client/sm"

	smClientTypes "github.com/SAP/sap-btp-service-operator/client/sm/types"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	secretNameTakenErrorFormat         = "the specified secret name '%s' is already taken. Choose another name and try again"
	secretAlreadyOwnedErrorFormat      = "secret %s belongs to another binding %s, choose a different name"
	secretTemplateSmBindingKey         = "smBindingCredentials"
	secretTemplateServiceInstanceInfos = "serviceInstanceInfos"
)

// ServiceBindingReconciler reconciles a ServiceBinding object
type ServiceBindingReconciler struct {
	*BaseReconciler
}

// +kubebuilder:rbac:groups=services.cloud.sap.com,resources=servicebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=services.cloud.sap.com,resources=servicebindings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=services.cloud.sap.com,resources=serviceinstances,verbs=get;list
// +kubebuilder:rbac:groups=services.cloud.sap.com,resources=serviceinstances/status,verbs=get
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;create;update

func (r *ServiceBindingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("servicebinding", req.NamespacedName).WithValues("correlation_id", uuid.New().String(), req.Name, req.Namespace)
	ctx = context.WithValue(ctx, LogKey{}, log)

	serviceBinding := &servicesv1.ServiceBinding{}
	if err := r.Client.Get(ctx, req.NamespacedName, serviceBinding); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "unable to fetch ServiceBinding")
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	serviceBinding = serviceBinding.DeepCopy()
	serviceBinding.SetObservedGeneration(serviceBinding.Generation)

	if len(serviceBinding.GetConditions()) == 0 {
		if err := r.init(ctx, serviceBinding); err != nil {
			return ctrl.Result{}, err
		}
	}

	serviceInstance, err := r.getServiceInstanceForBinding(ctx, serviceBinding)
	if client.IgnoreNotFound(err) != nil {
		log.Error(err, "failed to get service instance for binding")
		return ctrl.Result{}, err
	}

	if isMarkedForDeletion(serviceBinding.ObjectMeta) {
		return r.delete(ctx, serviceBinding, serviceInstance.Spec.BTPAccessCredentialsSecret)
	}

	if err != nil { // instance not found
		instanceNamespace := serviceBinding.Namespace
		if len(serviceBinding.Spec.ServiceInstanceNamespace) > 0 {
			instanceNamespace = serviceBinding.Spec.ServiceInstanceNamespace
		}
		errMsg := fmt.Sprintf("couldn't find the service instance '%s' in namespace '%s'", serviceBinding.Spec.ServiceInstanceName, instanceNamespace)
		setBlockedCondition(ctx, errMsg, serviceBinding)
		return ctrl.Result{}, r.updateStatus(ctx, serviceBinding)
	}

	if len(serviceBinding.Status.OperationURL) > 0 {
		// ongoing operation - poll status from SM
		return r.poll(ctx, serviceBinding, serviceInstance.Spec.BTPAccessCredentialsSecret)
	}

	if !controllerutil.ContainsFinalizer(serviceBinding, api.FinalizerName) {
		controllerutil.AddFinalizer(serviceBinding, api.FinalizerName)
		log.Info(fmt.Sprintf("added finalizer '%s' to service binding", api.FinalizerName))
		if err := r.Client.Update(ctx, serviceBinding); err != nil {
			return ctrl.Result{}, err
		}
	}

	isBindingReady := meta.IsStatusConditionPresentAndEqual(serviceBinding.Status.Conditions, api.ConditionReady, metav1.ConditionTrue)
	if isBindingReady {
		if isStaleServiceBinding(serviceBinding) {
			return r.handleStaleServiceBinding(ctx, serviceBinding)
		}

		if initCredRotationIfRequired(serviceBinding) {
			return ctrl.Result{}, r.updateStatus(ctx, serviceBinding)
		}
	}

	if meta.IsStatusConditionTrue(serviceBinding.Status.Conditions, api.ConditionCredRotationInProgress) {
		if err := r.rotateCredentials(ctx, serviceBinding, serviceInstance.Spec.BTPAccessCredentialsSecret); err != nil {
			return ctrl.Result{}, err
		}
	}

	if isBindingReady {
		log.Info("Binding in final state")
		return r.maintain(ctx, serviceBinding)
	}

	log.Info(fmt.Sprintf("Current generation is %v and observed is %v", serviceBinding.Generation, serviceBinding.GetObservedGeneration()))
	serviceBinding.SetObservedGeneration(serviceBinding.Generation)

	if serviceNotUsable(serviceInstance) {
		instanceErr := fmt.Errorf("service instance '%s' is not usable", serviceBinding.Spec.ServiceInstanceName)
		setBlockedCondition(ctx, instanceErr.Error(), serviceBinding)
		if err := r.updateStatus(ctx, serviceBinding); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, instanceErr
	}

	if isInProgress(serviceInstance) {
		log.Info(fmt.Sprintf("Service instance with k8s name %s is not ready for binding yet", serviceInstance.Name))
		setInProgressConditions(ctx, smClientTypes.CREATE, fmt.Sprintf("creation in progress, waiting for service instance '%s' to be ready", serviceBinding.Spec.ServiceInstanceName), serviceBinding)

		return ctrl.Result{Requeue: true, RequeueAfter: r.Config.PollInterval}, r.updateStatus(ctx, serviceBinding)
	}

	//set owner instance only for original bindings (not rotated)
	if serviceBinding.Labels == nil || len(serviceBinding.Labels[api.StaleBindingIDLabel]) == 0 {
		if !bindingAlreadyOwnedByInstance(serviceInstance, serviceBinding) &&
			serviceInstance.Namespace == serviceBinding.Namespace { //cross namespace reference not allowed
			if err := r.setOwner(ctx, serviceInstance, serviceBinding); err != nil {
				log.Error(err, "failed to set owner reference for binding")
				return ctrl.Result{}, err
			}
		}
	}

	if serviceBinding.Status.BindingID == "" {
		if err := r.validateSecretNameIsAvailable(ctx, serviceBinding); err != nil {
			setBlockedCondition(ctx, err.Error(), serviceBinding)
			return ctrl.Result{}, r.updateStatus(ctx, serviceBinding)
		}

		smClient, err := r.getSMClient(ctx, serviceBinding, serviceInstance.Spec.BTPAccessCredentialsSecret)
		if err != nil {
			return r.markAsTransientError(ctx, Unknown, err.Error(), serviceBinding)
		}

		smBinding, err := r.getBindingForRecovery(ctx, smClient, serviceBinding)
		if err != nil {
			log.Error(err, "failed to check binding recovery")
			return r.markAsTransientError(ctx, smClientTypes.CREATE, err.Error(), serviceBinding)
		}
		if smBinding != nil {
			return r.recover(ctx, serviceBinding, smBinding)
		}

		return r.createBinding(ctx, smClient, serviceInstance, serviceBinding)
	}

	log.Error(fmt.Errorf("update binding is not allowed, this line should not be reached"), "")
	return ctrl.Result{}, nil
}

func (r *ServiceBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&servicesv1.ServiceBinding{}).
		WithOptions(controller.Options{RateLimiter: workqueue.NewItemExponentialFailureRateLimiter(r.Config.RetryBaseDelay, r.Config.RetryMaxDelay)}).
		Complete(r)
}

func (r *ServiceBindingReconciler) createBinding(ctx context.Context, smClient sm.Client, serviceInstance *servicesv1.ServiceInstance, serviceBinding *servicesv1.ServiceBinding) (ctrl.Result, error) {
	log := GetLogger(ctx)
	log.Info("Creating smBinding in SM")
	serviceBinding.Status.InstanceID = serviceInstance.Status.InstanceID
	_, bindingParameters, err := buildParameters(r.Client, serviceBinding.Namespace, serviceBinding.Spec.ParametersFrom, serviceBinding.Spec.Parameters)
	if err != nil {
		log.Error(err, "failed to parse smBinding parameters")
		return r.markAsNonTransientError(ctx, smClientTypes.CREATE, err.Error(), serviceBinding)
	}

	smBinding, operationURL, bindErr := smClient.Bind(&smClientTypes.ServiceBinding{
		Name: serviceBinding.Spec.ExternalName,
		Labels: smClientTypes.Labels{
			namespaceLabel: []string{serviceBinding.Namespace},
			k8sNameLabel:   []string{serviceBinding.Name},
			clusterIDLabel: []string{r.Config.ClusterID},
		},
		ServiceInstanceID: serviceInstance.Status.InstanceID,
		Parameters:        bindingParameters,
	}, nil, buildUserInfo(ctx, serviceBinding.Spec.UserInfo))

	if bindErr != nil {
		log.Error(err, "failed to create service binding", "serviceInstanceID", serviceInstance.Status.InstanceID)
		return r.handleError(ctx, smClientTypes.CREATE, bindErr, serviceBinding)
	}

	if operationURL != "" {
		var bindingID string
		if bindingID = sm.ExtractBindingID(operationURL); len(bindingID) == 0 {
			return r.markAsNonTransientError(ctx, smClientTypes.CREATE, fmt.Sprintf("failed to extract smBinding ID from operation URL %s", operationURL), serviceBinding)
		}
		serviceBinding.Status.BindingID = bindingID

		log.Info("Create smBinding request is async")
		serviceBinding.Status.OperationURL = operationURL
		serviceBinding.Status.OperationType = smClientTypes.CREATE
		setInProgressConditions(ctx, smClientTypes.CREATE, "", serviceBinding)
		if err := r.updateStatus(ctx, serviceBinding); err != nil {
			log.Error(err, "unable to update ServiceBinding status")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true, RequeueAfter: r.Config.PollInterval}, nil
	}

	log.Info("Binding created successfully")

	if err := r.storeBindingSecret(ctx, serviceBinding, smBinding); err != nil {
		return r.handleSecretError(ctx, smClientTypes.CREATE, err, serviceBinding)
	}

	subaccountID := ""
	if len(smBinding.Labels["subaccount_id"]) > 0 {
		subaccountID = smBinding.Labels["subaccount_id"][0]
	}

	serviceBinding.Status.BindingID = smBinding.ID
	serviceBinding.Status.SubaccountID = subaccountID
	serviceBinding.Status.Ready = metav1.ConditionTrue
	setSuccessConditions(smClientTypes.CREATE, serviceBinding)
	log.Info("Updating binding", "bindingID", smBinding.ID)

	return ctrl.Result{}, r.updateStatus(ctx, serviceBinding)
}

func (r *ServiceBindingReconciler) delete(ctx context.Context, serviceBinding *servicesv1.ServiceBinding, btpAccessCredentialsSecret string) (ctrl.Result, error) {
	log := GetLogger(ctx)
	if controllerutil.ContainsFinalizer(serviceBinding, api.FinalizerName) {
		smClient, err := r.getSMClient(ctx, serviceBinding, btpAccessCredentialsSecret)
		if err != nil {
			return r.markAsTransientError(ctx, Unknown, err.Error(), serviceBinding)
		}

		if len(serviceBinding.Status.BindingID) == 0 {
			log.Info("No binding id found validating binding does not exists in SM before removing finalizer")
			smBinding, err := r.getBindingForRecovery(ctx, smClient, serviceBinding)
			if err != nil {
				return ctrl.Result{}, err
			}
			if smBinding != nil {
				log.Info("binding exists in SM continue with deletion")
				serviceBinding.Status.BindingID = smBinding.ID
				setInProgressConditions(ctx, smClientTypes.DELETE, "delete after recovery", serviceBinding)
				return ctrl.Result{}, r.updateStatus(ctx, serviceBinding)
			}

			// make sure there's no secret stored for the binding
			if err := r.deleteBindingSecret(ctx, serviceBinding); err != nil {
				return ctrl.Result{}, err
			}

			log.Info("Binding does not exists in SM, removing finalizer")
			if err := r.removeFinalizer(ctx, serviceBinding, api.FinalizerName); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}

		if len(serviceBinding.Status.OperationURL) > 0 && serviceBinding.Status.OperationType == smClientTypes.DELETE {
			// ongoing delete operation - poll status from SM
			return r.poll(ctx, serviceBinding, btpAccessCredentialsSecret)
		}

		log.Info(fmt.Sprintf("Deleting binding with id %v from SM", serviceBinding.Status.BindingID))
		operationURL, unbindErr := smClient.Unbind(serviceBinding.Status.BindingID, nil, buildUserInfo(ctx, serviceBinding.Spec.UserInfo))
		if unbindErr != nil {
			// delete will proceed anyway
			return r.markAsNonTransientError(ctx, smClientTypes.DELETE, unbindErr.Error(), serviceBinding)
		}

		if operationURL != "" {
			log.Info("Deleting binding async")
			serviceBinding.Status.OperationURL = operationURL
			serviceBinding.Status.OperationType = smClientTypes.DELETE
			setInProgressConditions(ctx, smClientTypes.DELETE, "", serviceBinding)
			if err := r.updateStatus(ctx, serviceBinding); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true, RequeueAfter: r.Config.PollInterval}, nil
		}

		log.Info("Binding was deleted successfully")
		return r.deleteSecretAndRemoveFinalizer(ctx, serviceBinding)
	}
	return ctrl.Result{}, nil
}

func (r *ServiceBindingReconciler) poll(ctx context.Context, serviceBinding *servicesv1.ServiceBinding, btpAccessCredentialsSecret string) (ctrl.Result, error) {
	log := GetLogger(ctx)
	log.Info(fmt.Sprintf("resource is in progress, found operation url %s", serviceBinding.Status.OperationURL))

	smClient, err := r.getSMClient(ctx, serviceBinding, btpAccessCredentialsSecret)
	if err != nil {
		return r.markAsTransientError(ctx, Unknown, err.Error(), serviceBinding)
	}

	status, statusErr := smClient.Status(serviceBinding.Status.OperationURL, nil)
	if statusErr != nil {
		log.Info(fmt.Sprintf("failed to fetch operation, got error from SM: %s", statusErr.Error()), "operationURL", serviceBinding.Status.OperationURL)
		setFailureConditions(serviceBinding.Status.OperationType, statusErr.Error(), serviceBinding)
		freshStatus := servicesv1.ServiceBindingStatus{
			Conditions: serviceBinding.GetConditions(),
		}
		if isMarkedForDeletion(serviceBinding.ObjectMeta) {
			freshStatus.BindingID = serviceBinding.Status.BindingID
		}
		serviceBinding.Status = freshStatus
		if err := r.updateStatus(ctx, serviceBinding); err != nil {
			log.Error(err, "failed to update status during polling")
		}
		return ctrl.Result{}, statusErr
	}

	if status == nil {
		return r.markAsTransientError(ctx, serviceBinding.Status.OperationType, fmt.Sprintf("failed to get last operation status of %s", serviceBinding.Name), serviceBinding)
	}
	switch status.State {
	case smClientTypes.INPROGRESS:
		fallthrough
	case smClientTypes.PENDING:
		if len(status.Description) != 0 {
			setInProgressConditions(ctx, status.Type, status.Description, serviceBinding)
			if err := r.updateStatus(ctx, serviceBinding); err != nil {
				log.Error(err, "unable to update ServiceBinding polling description")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{Requeue: true, RequeueAfter: r.Config.PollInterval}, nil
	case smClientTypes.FAILED:
		// non transient error - should not retry
		setFailureConditions(status.Type, status.Description, serviceBinding)
		if serviceBinding.Status.OperationType == smClientTypes.DELETE {
			serviceBinding.Status.OperationURL = ""
			serviceBinding.Status.OperationType = ""
			if err := r.updateStatus(ctx, serviceBinding); err != nil {
				log.Error(err, "unable to update ServiceBinding status")
				return ctrl.Result{}, err
			}
			errMsg := "Async unbind operation failed"
			if status.Errors != nil {
				errMsg = fmt.Sprintf("Async unbind operation failed, errors: %s", string(status.Errors))
			}
			return ctrl.Result{}, fmt.Errorf(errMsg)
		}
	case smClientTypes.SUCCEEDED:
		setSuccessConditions(status.Type, serviceBinding)
		switch serviceBinding.Status.OperationType {
		case smClientTypes.CREATE:
			smBinding, err := smClient.GetBindingByID(serviceBinding.Status.BindingID, nil)
			if err != nil {
				log.Error(err, fmt.Sprintf("binding %s succeeded but could not fetch it from SM", serviceBinding.Status.BindingID))
				return ctrl.Result{}, err
			}
			if len(smBinding.Labels["subaccount_id"]) > 0 {
				serviceBinding.Status.SubaccountID = smBinding.Labels["subaccount_id"][0]
			}

			if err := r.storeBindingSecret(ctx, serviceBinding, smBinding); err != nil {
				return r.handleSecretError(ctx, smClientTypes.CREATE, err, serviceBinding)
			}
			setSuccessConditions(status.Type, serviceBinding)
		case smClientTypes.DELETE:
			return r.deleteSecretAndRemoveFinalizer(ctx, serviceBinding)
		}
	}

	serviceBinding.Status.OperationURL = ""
	serviceBinding.Status.OperationType = ""

	return ctrl.Result{}, r.updateStatus(ctx, serviceBinding)
}

func (r *ServiceBindingReconciler) getBindingForRecovery(ctx context.Context, smClient sm.Client, serviceBinding *servicesv1.ServiceBinding) (*smClientTypes.ServiceBinding, error) {
	log := GetLogger(ctx)
	nameQuery := fmt.Sprintf("name eq '%s'", serviceBinding.Spec.ExternalName)
	clusterIDQuery := fmt.Sprintf("context/clusterid eq '%s'", r.Config.ClusterID)
	namespaceQuery := fmt.Sprintf("context/namespace eq '%s'", serviceBinding.Namespace)
	k8sNameQuery := fmt.Sprintf("%s eq '%s'", k8sNameLabel, serviceBinding.Name)
	parameters := sm.Parameters{
		FieldQuery:    []string{nameQuery, clusterIDQuery, namespaceQuery},
		LabelQuery:    []string{k8sNameQuery},
		GeneralParams: []string{"attach_last_operations=true"},
	}
	log.Info(fmt.Sprintf("binding recovery query params: %s, %s, %s, %s", nameQuery, clusterIDQuery, namespaceQuery, k8sNameQuery))

	bindings, err := smClient.ListBindings(&parameters)
	if err != nil {
		log.Error(err, "failed to list bindings in SM")
		return nil, err
	}
	if bindings != nil {
		log.Info(fmt.Sprintf("found %d bindings", len(bindings.ServiceBindings)))
		if len(bindings.ServiceBindings) == 1 {
			return &bindings.ServiceBindings[0], nil
		}
	}
	return nil, nil
}

func (r *ServiceBindingReconciler) maintain(ctx context.Context, binding *servicesv1.ServiceBinding) (ctrl.Result, error) {
	log := GetLogger(ctx)
	shouldUpdateStatus := false
	if binding.Generation != binding.Status.ObservedGeneration {
		binding.SetObservedGeneration(binding.Generation)
		shouldUpdateStatus = true
	}

	if !isFailed(binding) {
		if _, err := r.getSecret(ctx, binding.Namespace, binding.Spec.SecretName); err != nil {
			if apierrors.IsNotFound(err) && !isMarkedForDeletion(binding.ObjectMeta) {
				log.Info(fmt.Sprintf("secret not found recovering binding %s", binding.Name))
				binding.Status.BindingID = ""
				binding.Status.Ready = metav1.ConditionFalse
				setInProgressConditions(ctx, smClientTypes.CREATE, "recreating deleted secret", binding)
				shouldUpdateStatus = true
				r.Recorder.Event(binding, corev1.EventTypeWarning, "SecretDeleted", "SecretDeleted")
			} else {
				return ctrl.Result{}, err
			}
		}
	}

	if shouldUpdateStatus {
		log.Info(fmt.Sprintf("maintanance required for binding %s", binding.Name))
		return ctrl.Result{}, r.updateStatus(ctx, binding)
	}

	return ctrl.Result{}, nil
}

func (r *ServiceBindingReconciler) getServiceInstanceForBinding(ctx context.Context, binding *servicesv1.ServiceBinding) (*servicesv1.ServiceInstance, error) {
	log := GetLogger(ctx)
	serviceInstance := &servicesv1.ServiceInstance{}
	namespace := binding.Namespace
	if len(binding.Spec.ServiceInstanceNamespace) > 0 {
		namespace = binding.Spec.ServiceInstanceNamespace
	}
	log.Info(fmt.Sprintf("getting service instance named %s in namespace %s for binding %s in namespace %s", binding.Spec.ServiceInstanceName, namespace, binding.Name, binding.Namespace))
	if err := r.Client.Get(ctx, types.NamespacedName{Name: binding.Spec.ServiceInstanceName, Namespace: namespace}, serviceInstance); err != nil {
		return nil, err
	}

	return serviceInstance.DeepCopy(), nil
}

func (r *ServiceBindingReconciler) setOwner(ctx context.Context, serviceInstance *servicesv1.ServiceInstance, serviceBinding *servicesv1.ServiceBinding) error {
	log := GetLogger(ctx)
	log.Info("Binding instance as owner of binding", "bindingName", serviceBinding.Name, "instanceName", serviceInstance.Name)
	if err := controllerutil.SetControllerReference(serviceInstance, serviceBinding, r.Scheme); err != nil {
		log.Error(err, fmt.Sprintf("Could not update the smBinding %s owner instance reference", serviceBinding.Name))
		return err
	}
	if err := r.Client.Update(ctx, serviceBinding); err != nil {
		log.Error(err, "Failed to set controller reference", "bindingName", serviceBinding.Name)
		return err
	}
	return nil
}

func (r *ServiceBindingReconciler) resyncBindingStatus(ctx context.Context, k8sBinding *servicesv1.ServiceBinding, smBinding *smClientTypes.ServiceBinding) {
	k8sBinding.Status.ObservedGeneration = k8sBinding.Generation
	k8sBinding.Status.BindingID = smBinding.ID
	k8sBinding.Status.InstanceID = smBinding.ServiceInstanceID
	k8sBinding.Status.OperationURL = ""
	k8sBinding.Status.OperationType = ""

	bindingStatus := smClientTypes.SUCCEEDED
	operationType := smClientTypes.CREATE
	description := ""
	if smBinding.LastOperation != nil {
		bindingStatus = smBinding.LastOperation.State
		operationType = smBinding.LastOperation.Type
		description = smBinding.LastOperation.Description
	} else if !smBinding.Ready {
		bindingStatus = smClientTypes.FAILED
	}
	switch bindingStatus {
	case smClientTypes.PENDING:
		fallthrough
	case smClientTypes.INPROGRESS:
		k8sBinding.Status.OperationURL = sm.BuildOperationURL(smBinding.LastOperation.ID, smBinding.ID, smClientTypes.ServiceBindingsURL)
		k8sBinding.Status.OperationType = smBinding.LastOperation.Type
		setInProgressConditions(ctx, smBinding.LastOperation.Type, smBinding.LastOperation.Description, k8sBinding)
	case smClientTypes.SUCCEEDED:
		setSuccessConditions(operationType, k8sBinding)
	case smClientTypes.FAILED:
		setFailureConditions(operationType, description, k8sBinding)
	}
}

func (r *ServiceBindingReconciler) storeBindingSecret(ctx context.Context, k8sBinding *servicesv1.ServiceBinding, smBinding *smClientTypes.ServiceBinding) error {
	log := GetLogger(ctx)
	logger := log.WithValues("bindingName", k8sBinding.Name, "secretName", k8sBinding.Spec.SecretName)
	var secret *corev1.Secret
	var err error

	if k8sBinding.Spec.SecretTemplate != "" {
		secret, err = r.createBindingSecretFromSecretTemplate(ctx, k8sBinding, smBinding.Credentials)
	} else {
		secret, err = r.createBindingSecret(ctx, k8sBinding, smBinding.Credentials)
	}

	if err != nil {
		return err
	}

	if err := controllerutil.SetControllerReference(k8sBinding, secret, r.Scheme); err != nil {
		logger.Error(err, "Failed to set secret owner")
		return err
	}

	return r.createOrUpdateBindingSecret(ctx, k8sBinding, secret)
}

// createBindingSecretFromSecretTemplate executes the template of .Spec.SecretTemplate
func (r *ServiceBindingReconciler) createBindingSecretFromSecretTemplate(ctx context.Context, k8sBinding *servicesv1.ServiceBinding, inputSmCredentials json.RawMessage) (*corev1.Secret, error) {
	log := GetLogger(ctx)
	log.Info("Create Object using SecretTemplate from ServiceBinding Specs")

	smBindingCredentials := make(map[string]interface{})
	if inputSmCredentials != nil {
		err := json.Unmarshal(inputSmCredentials, &smBindingCredentials)
		if err != nil {
			return nil, errors.Wrap(err, "failed to unmarshal given service binding credentials")
		}
	}

	instanceInfos := make(map[string][]byte)
	_, err := r.addInstanceInfo(ctx, k8sBinding, instanceInfos)
	if err != nil {
		return nil, errors.Wrap(err, "failed to add service instance info")
	}

	//convert the bytes to string to ensure, that the secret can be created later by CreateSecretFromTemplate
	convertedInstanceInfos := make(map[string]string)
	for k, v := range instanceInfos {
		convertedInstanceInfos[k] = string(v)
	}

	parameters := map[string]interface{}{
		secretTemplateSmBindingKey:         smBindingCredentials,
		secretTemplateServiceInstanceInfos: convertedInstanceInfos,
	}

	templateName := fmt.Sprintf("%s/%s", k8sBinding.Namespace, k8sBinding.Name)
	secret, err := template.CreateSecretFromTemplate(templateName, k8sBinding.Spec.SecretTemplate, parameters)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create secret from template")
	}

	secret.SetNamespace(k8sBinding.Namespace)
	secret.SetName(k8sBinding.Spec.SecretName)

	return secret, nil
}

func (r *ServiceBindingReconciler) createBindingSecret(ctx context.Context, k8sBinding *servicesv1.ServiceBinding, credentials json.RawMessage) (*corev1.Secret, error) {
	log := GetLogger(ctx)
	logger := log.WithValues("bindingName", k8sBinding.Name, "secretName", k8sBinding.Spec.SecretName)
	var credentialsMap map[string][]byte
	var credentialProperties []SecretMetadataProperty

	if len(credentials) == 0 {
		log.Info("Binding credentials are empty")
		credentialsMap = make(map[string][]byte)
	} else if k8sBinding.Spec.SecretKey != nil {
		credentialsMap = map[string][]byte{
			*k8sBinding.Spec.SecretKey: credentials,
		}
		credentialProperties = []SecretMetadataProperty{
			{
				Name:      *k8sBinding.Spec.SecretKey,
				Format:    string(JSON),
				Container: true,
			},
		}
	} else {
		var err error
		credentialsMap, credentialProperties, err = normalizeCredentials(credentials)
		if err != nil {
			logger.Error(err, "Failed to store binding secret")
			return nil, fmt.Errorf("failed to create secret. Error: %v", err.Error())
		}
	}

	metaDataProperties, err := r.addInstanceInfo(ctx, k8sBinding, credentialsMap)
	if err != nil {
		log.Error(err, "failed to enrich binding with service instance info")
	}

	if k8sBinding.Spec.SecretRootKey != nil {
		var err error
		credentialsMap, err = singleKeyMap(credentialsMap, *k8sBinding.Spec.SecretRootKey)
		if err != nil {
			return nil, err
		}
	} else {
		metadata := map[string][]SecretMetadataProperty{
			"metaDataProperties":   metaDataProperties,
			"credentialProperties": credentialProperties,
		}
		metadataByte, err := json.Marshal(metadata)
		if err != nil {
			log.Error(err, "failed to enrich binding with metadata")
		} else {
			credentialsMap[".metadata"] = metadataByte
		}
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        k8sBinding.Spec.SecretName,
			Annotations: map[string]string{"binding": k8sBinding.Name},
			Namespace:   k8sBinding.Namespace,
		},
		Data: credentialsMap,
	}

	return secret, nil
}

func (r *ServiceBindingReconciler) createOrUpdateBindingSecret(ctx context.Context, binding *servicesv1.ServiceBinding, secret *corev1.Secret) error {
	log := GetLogger(ctx)
	dbSecret := &corev1.Secret{}
	create := false
	if err := r.Client.Get(ctx, types.NamespacedName{Name: binding.Spec.SecretName, Namespace: binding.Namespace}, dbSecret); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		create = true
	}

	if create {
		log.Info("Creating binding secret", "name", secret.Name)
		if err := r.Client.Create(ctx, secret); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return err
			}
			return nil
		}
		r.Recorder.Event(binding, corev1.EventTypeNormal, "SecretCreated", "SecretCreated")
		return nil
	}

	log.Info("Updating existing binding secret", "name", secret.Name)
	dbSecret.Data = secret.Data
	dbSecret.StringData = secret.StringData
	return r.Client.Update(ctx, dbSecret)
}

func (r *ServiceBindingReconciler) deleteBindingSecret(ctx context.Context, binding *servicesv1.ServiceBinding) error {
	log := GetLogger(ctx)
	log.Info("Deleting binding secret")
	bindingSecret := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: binding.Namespace,
		Name:      binding.Spec.SecretName,
	}, bindingSecret); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "unable to fetch binding secret")
			return err
		}

		// secret not found, nothing more to do
		log.Info("secret was deleted successfully")
		return nil
	}
	bindingSecret = bindingSecret.DeepCopy()

	if err := r.Client.Delete(ctx, bindingSecret); err != nil {
		log.Error(err, "Failed to delete binding secret")
		return err
	}

	log.Info("secret was deleted successfully")
	return nil
}

func (r *ServiceBindingReconciler) deleteSecretAndRemoveFinalizer(ctx context.Context, serviceBinding *servicesv1.ServiceBinding) (ctrl.Result, error) {
	// delete binding secret if exist
	if err := r.deleteBindingSecret(ctx, serviceBinding); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, r.removeFinalizer(ctx, serviceBinding, api.FinalizerName)
}

func (r *ServiceBindingReconciler) getSecret(ctx context.Context, namespace string, name string) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, secret)
	return secret, err
}

func (r *ServiceBindingReconciler) validateSecretNameIsAvailable(ctx context.Context, binding *servicesv1.ServiceBinding) error {
	currentSecret, err := r.getSecret(ctx, binding.Namespace, binding.Spec.SecretName)
	if err != nil {
		return client.IgnoreNotFound(err)
	}

	if metav1.IsControlledBy(currentSecret, binding) {
		return nil
	}

	ownerRef := metav1.GetControllerOf(currentSecret)
	if ownerRef != nil {
		owner, err := schema.ParseGroupVersion(ownerRef.APIVersion)
		if err != nil {
			return err
		}

		if owner.Group == binding.GroupVersionKind().Group && ownerRef.Kind == binding.Kind {
			return fmt.Errorf(secretAlreadyOwnedErrorFormat, binding.Spec.SecretName, ownerRef.Name)
		}
	}

	return fmt.Errorf(secretNameTakenErrorFormat, binding.Spec.SecretName)
}

func (r *ServiceBindingReconciler) handleSecretError(ctx context.Context, op smClientTypes.OperationCategory, err error, binding *servicesv1.ServiceBinding) (ctrl.Result, error) {
	log := GetLogger(ctx)
	log.Error(err, fmt.Sprintf("failed to store secret %s for binding %s", binding.Spec.SecretName, binding.Name))
	if apierrors.ReasonForError(err) == metav1.StatusReasonUnknown {
		return r.markAsNonTransientError(ctx, op, err.Error(), binding)
	}
	return r.markAsTransientError(ctx, op, err.Error(), binding)
}

func (r *ServiceBindingReconciler) addInstanceInfo(ctx context.Context, binding *servicesv1.ServiceBinding, credentialsMap map[string][]byte) ([]SecretMetadataProperty, error) {
	instance, err := r.getServiceInstanceForBinding(ctx, binding)
	if err != nil {
		return nil, err
	}

	credentialsMap["instance_name"] = getInstanceNameForSecretCredentials(instance)
	credentialsMap["instance_guid"] = []byte(instance.Status.InstanceID)
	credentialsMap["plan"] = []byte(instance.Spec.ServicePlanName)
	credentialsMap["label"] = []byte(instance.Spec.ServiceOfferingName)
	credentialsMap["type"] = []byte(instance.Spec.ServiceOfferingName)
	if len(instance.Status.Tags) > 0 || len(instance.Spec.CustomTags) > 0 {
		tagsBytes, err := json.Marshal(mergeInstanceTags(instance.Status.Tags, instance.Spec.CustomTags))
		if err != nil {
			return nil, err
		}
		credentialsMap["tags"] = tagsBytes
	}

	metadata := []SecretMetadataProperty{
		{
			Name:   "instance_name",
			Format: string(TEXT),
		},
		{
			Name:   "instance_guid",
			Format: string(TEXT),
		},
		{
			Name:   "plan",
			Format: string(TEXT),
		},
		{
			Name:   "label",
			Format: string(TEXT),
		},
		{
			Name:   "type",
			Format: string(TEXT),
		},
	}
	if _, ok := credentialsMap["tags"]; ok {
		metadata = append(metadata, SecretMetadataProperty{Name: "tags", Format: string(JSON)})
	}

	return metadata, nil
}

func (r *ServiceBindingReconciler) rotateCredentials(ctx context.Context, binding *servicesv1.ServiceBinding, btpAccessCredentialsSecret string) error {
	suffix := "-" + RandStringRunes(6)
	log := GetLogger(ctx)
	if binding.Annotations != nil {
		if _, ok := binding.Annotations[api.ForceRotateAnnotation]; ok {
			log.Info("Credentials rotation - deleting force rotate annotation")
			delete(binding.Annotations, api.ForceRotateAnnotation)
			if err := r.Client.Update(ctx, binding); err != nil {
				log.Info("Credentials rotation - failed to delete force rotate annotation")
				return err
			}
		}
	}

	credInProgressCondition := meta.FindStatusCondition(binding.GetConditions(), api.ConditionCredRotationInProgress)
	if credInProgressCondition.Reason == CredRotating {
		if len(binding.Status.BindingID) > 0 && binding.Status.Ready == metav1.ConditionTrue {
			log.Info("Credentials rotation - finished successfully")
			now := metav1.NewTime(time.Now())
			binding.Status.LastCredentialsRotationTime = &now
			return r.stopRotation(ctx, binding)
		} else if isFailed(binding) {
			log.Info("Credentials rotation - binding failed stopping rotation")
			return r.stopRotation(ctx, binding)
		}
		log.Info("Credentials rotation - waiting to finish")
		return nil
	}

	if len(binding.Status.BindingID) == 0 {
		log.Info("Credentials rotation - no binding id found nothing to do")
		return r.stopRotation(ctx, binding)
	}

	bindings := &servicesv1.ServiceBindingList{}
	err := r.Client.List(ctx, bindings, client.MatchingLabels{api.StaleBindingIDLabel: binding.Status.BindingID}, client.InNamespace(binding.Namespace))
	if err != nil {
		return err
	}

	if len(bindings.Items) == 0 {
		smClient, err := r.getSMClient(ctx, binding, btpAccessCredentialsSecret)
		if err != nil {
			return err
		}

		// rename current binding
		log.Info("Credentials rotation - renaming binding to old in SM", "current", binding.Spec.ExternalName)
		if _, errRenaming := smClient.RenameBinding(binding.Status.BindingID, binding.Spec.ExternalName+suffix, binding.Name+suffix); errRenaming != nil {
			log.Error(errRenaming, "Credentials rotation - failed renaming binding to old in SM", "binding", binding.Spec.ExternalName)
			setCredRotationInProgressConditions(CredPreparing, errRenaming.Error(), binding)
			if errStatus := r.updateStatus(ctx, binding); errStatus != nil {
				return errStatus
			}
			return errRenaming
		}

		log.Info("Credentials rotation - backing up old binding in K8S", "name", binding.Name+suffix)
		if err := r.createOldBinding(ctx, suffix, binding); err != nil {
			log.Error(err, "Credentials rotation - failed to back up old binding in K8S")

			setCredRotationInProgressConditions(CredPreparing, err.Error(), binding)
			if errStatus := r.updateStatus(ctx, binding); errStatus != nil {
				return errStatus
			}
			return err
		}
	}

	binding.Status.BindingID = ""
	binding.Status.Ready = metav1.ConditionFalse
	setInProgressConditions(ctx, smClientTypes.CREATE, "rotating binding credentials", binding)
	setCredRotationInProgressConditions(CredRotating, "", binding)
	return r.updateStatus(ctx, binding)
}

func (r *ServiceBindingReconciler) stopRotation(ctx context.Context, binding *servicesv1.ServiceBinding) error {
	conditions := binding.GetConditions()
	meta.RemoveStatusCondition(&conditions, api.ConditionCredRotationInProgress)
	binding.Status.Conditions = conditions
	return r.updateStatus(ctx, binding)
}

func (r *ServiceBindingReconciler) createOldBinding(ctx context.Context, suffix string, binding *servicesv1.ServiceBinding) error {
	oldBinding := newBindingObject(binding.Name+suffix, binding.Namespace)
	err := controllerutil.SetControllerReference(binding, oldBinding, r.Scheme)
	if err != nil {
		return err
	}
	oldBinding.Labels = map[string]string{
		api.StaleBindingIDLabel:         binding.Status.BindingID,
		api.StaleBindingRotationOfLabel: binding.Name,
	}
	spec := binding.Spec.DeepCopy()
	spec.CredRotationPolicy.Enabled = false
	spec.SecretName = spec.SecretName + suffix
	spec.ExternalName = spec.ExternalName + suffix
	oldBinding.Spec = *spec
	return r.Client.Create(ctx, oldBinding)
}

func (r *ServiceBindingReconciler) handleStaleServiceBinding(ctx context.Context, serviceBinding *servicesv1.ServiceBinding) (ctrl.Result, error) {
	log := GetLogger(ctx)
	originalBindingName, ok := serviceBinding.Labels[api.StaleBindingRotationOfLabel]
	if !ok {
		//if the user removed the "rotationOf" label the stale binding should be deleted otherwise it will remain forever
		log.Info("missing rotationOf label, unable to fetch original binding, deleting stale")
		return ctrl.Result{}, r.Client.Delete(ctx, serviceBinding)
	}
	origBinding := &servicesv1.ServiceBinding{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: serviceBinding.Namespace, Name: originalBindingName}, origBinding); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("original binding not found, deleting stale binding")
			return ctrl.Result{}, r.Client.Delete(ctx, serviceBinding)
		}
		return ctrl.Result{}, err
	}
	if meta.IsStatusConditionTrue(origBinding.Status.Conditions, api.ConditionReady) {
		return ctrl.Result{}, r.Client.Delete(ctx, serviceBinding)
	}

	log.Info("not deleting stale binding since original binding is not ready")
	if !meta.IsStatusConditionPresentAndEqual(serviceBinding.Status.Conditions, api.ConditionPendingTermination, metav1.ConditionTrue) {
		pendingTerminationCondition := metav1.Condition{
			Type:               api.ConditionPendingTermination,
			Status:             metav1.ConditionTrue,
			Reason:             api.ConditionPendingTermination,
			Message:            "waiting for new credentials to be ready",
			ObservedGeneration: serviceBinding.GetGeneration(),
		}
		meta.SetStatusCondition(&serviceBinding.Status.Conditions, pendingTerminationCondition)
		return ctrl.Result{}, r.updateStatus(ctx, serviceBinding)
	}
	return ctrl.Result{}, nil
}

func (r *ServiceBindingReconciler) recover(ctx context.Context, serviceBinding *servicesv1.ServiceBinding, smBinding *smClientTypes.ServiceBinding) (ctrl.Result, error) {
	log := GetLogger(ctx)
	log.Info(fmt.Sprintf("found existing smBinding in SM with id %s, updating status", smBinding.ID))

	if smBinding.Credentials != nil {
		if err := r.storeBindingSecret(ctx, serviceBinding, smBinding); err != nil {
			operationType := smClientTypes.CREATE
			if smBinding.LastOperation != nil {
				operationType = smBinding.LastOperation.Type
			}
			return r.handleSecretError(ctx, operationType, err, serviceBinding)
		}
	}
	r.resyncBindingStatus(ctx, serviceBinding, smBinding)

	return ctrl.Result{}, r.updateStatus(ctx, serviceBinding)
}

func isStaleServiceBinding(binding *servicesv1.ServiceBinding) bool {
	if isMarkedForDeletion(binding.ObjectMeta) {
		return false
	}

	if binding.Labels != nil {
		if _, ok := binding.Labels[api.StaleBindingIDLabel]; ok {
			if binding.Spec.CredRotationPolicy != nil {
				keepFor, _ := time.ParseDuration(binding.Spec.CredRotationPolicy.RotatedBindingTTL)
				if time.Since(binding.CreationTimestamp.Time) > keepFor {
					return true
				}
			}
		}
	}
	return false
}

func initCredRotationIfRequired(binding *servicesv1.ServiceBinding) bool {
	if isFailed(binding) || !credRotationEnabled(binding) || meta.IsStatusConditionTrue(binding.Status.Conditions, api.ConditionCredRotationInProgress) {
		return false
	}
	_, forceRotate := binding.Annotations[api.ForceRotateAnnotation]

	lastCredentialRotationTime := binding.Status.LastCredentialsRotationTime
	if lastCredentialRotationTime == nil {
		ts := metav1.NewTime(binding.CreationTimestamp.Time)
		lastCredentialRotationTime = &ts
	}

	rotationInterval, _ := time.ParseDuration(binding.Spec.CredRotationPolicy.RotationFrequency)
	if time.Since(lastCredentialRotationTime.Time) > rotationInterval || forceRotate {
		setCredRotationInProgressConditions(CredPreparing, "", binding)
		return true
	}

	return false
}

func credRotationEnabled(binding *servicesv1.ServiceBinding) bool {
	return binding.Spec.CredRotationPolicy != nil && binding.Spec.CredRotationPolicy.Enabled
}

func mergeInstanceTags(offeringTags, customTags []string) []string {
	var tags []string

	for _, tag := range append(offeringTags, customTags...) {
		if !contains(tags, tag) {
			tags = append(tags, tag)
		}
	}
	return tags
}

func newBindingObject(name, namespace string) *servicesv1.ServiceBinding {
	return &servicesv1.ServiceBinding{
		TypeMeta: metav1.TypeMeta{
			APIVersion: servicesv1.GroupVersion.String(),
			Kind:       "ServiceBinding",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
}

func bindingAlreadyOwnedByInstance(instance *servicesv1.ServiceInstance, binding *servicesv1.ServiceBinding) bool {
	if existing := metav1.GetControllerOf(binding); existing != nil {
		aGV, err := schema.ParseGroupVersion(existing.APIVersion)
		if err != nil {
			return false
		}

		bGV, err := schema.ParseGroupVersion(instance.APIVersion)
		if err != nil {
			return false
		}

		return aGV.Group == bGV.Group && existing.Kind == instance.Kind && existing.Name == instance.Name
	}
	return false
}

func serviceNotUsable(instance *servicesv1.ServiceInstance) bool {
	if isMarkedForDeletion(instance.ObjectMeta) {
		return true
	}
	if len(instance.Status.Conditions) != 0 {
		return instance.Status.Conditions[0].Reason == getConditionReason(smClientTypes.CREATE, smClientTypes.FAILED)
	}
	return false
}

func getInstanceNameForSecretCredentials(instance *servicesv1.ServiceInstance) []byte {
	if useMetaName, ok := instance.Annotations[api.UseInstanceMetadataNameInSecret]; ok && useMetaName == "true" {
		return []byte(instance.Name)
	}
	return []byte(instance.Spec.ExternalName)
}

func singleKeyMap(credentialsMap map[string][]byte, key string) (map[string][]byte, error) {
	stringCredentialsMap := make(map[string]string)
	for k, v := range credentialsMap {
		stringCredentialsMap[k] = string(v)
	}

	credBytes, err := json.Marshal(stringCredentialsMap)
	if err != nil {
		return nil, err
	}

	return map[string][]byte{
		key: credBytes,
	}, nil
}
