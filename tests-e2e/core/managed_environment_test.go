package core

import (
	"context"
	"fmt"
	"os"

	appv1alpha1 "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appstudioshared "github.com/redhat-appstudio/application-api/api/v1alpha1"
	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/db"
	dbutil "github.com/redhat-appstudio/managed-gitops/backend-shared/db/util"
	argocdutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util/argocd"
	clusteragenteventloop "github.com/redhat-appstudio/managed-gitops/cluster-agent/controllers/managed-gitops/eventloop"
	"github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture"
	appFixture "github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/application"
	gitopsDeplFixture "github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/gitopsdeployment"
	"github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/k8s"
	"github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/managedenvironment"
	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("GitOpsDeployment Managed Environment E2E tests", func() {

	Context("Create a new GitOpsDeployment targeting a ManagedEnvironment", func() {

		ctx := context.Background()

		BeforeEach(func() {
			Expect(fixture.EnsureCleanSlate()).To(Succeed())
		})

		It("should be healthy and have synced status, and resources should be deployed, when deployed with a ManagedEnv", func() {

			if fixture.IsRunningAgainstKCP() {
				Skip("Skipping this test until we support running gitops operator with KCP")
			}

			Expect(fixture.EnsureCleanSlate()).To(Succeed())

			By("creating the GitOpsDeploymentManagedEnvironment")

			kubeConfigContents, apiServerURL, err := fixture.ExtractKubeConfigValues()
			Expect(err).To(BeNil())

			managedEnv, secret := buildManagedEnvironment(apiServerURL, kubeConfigContents, true)

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			err = k8s.Create(&secret, k8sClient)
			Expect(err).To(BeNil())

			err = k8s.Create(&managedEnv, k8sClient)
			Expect(err).To(BeNil())

			gitOpsDeploymentResource := buildGitOpsDeploymentResource("my-gitops-depl",
				"https://github.com/redhat-appstudio/managed-gitops", "resources/test-data/sample-gitops-repository/environments/overlays/dev",
				managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated)
			gitOpsDeploymentResource.Spec.Destination.Environment = managedEnv.Name
			gitOpsDeploymentResource.Spec.Destination.Namespace = fixture.GitOpsServiceE2ENamespace
			err = k8s.Create(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(BeNil())

			By("ensuring GitOpsDeployment should have expected health and status and reconciledState")

			expectedReconciledStateField := managedgitopsv1alpha1.ReconciledState{
				Source: managedgitopsv1alpha1.GitOpsDeploymentSource{
					RepoURL: gitOpsDeploymentResource.Spec.Source.RepoURL,
					Path:    gitOpsDeploymentResource.Spec.Source.Path,
				},
				Destination: managedgitopsv1alpha1.GitOpsDeploymentDestination{
					Name:      gitOpsDeploymentResource.Spec.Destination.Environment,
					Namespace: gitOpsDeploymentResource.Spec.Destination.Namespace,
				},
			}

			Eventually(gitOpsDeploymentResource, "2m", "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy),
					gitopsDeplFixture.HaveReconciledState(expectedReconciledStateField)))

			secretList := corev1.SecretList{}

			err = k8sClient.List(context.Background(), &secretList, &client.ListOptions{Namespace: dbutil.DefaultGitOpsEngineSingleInstanceNamespace})
			Expect(err).To(BeNil())

			dbQueries, err := db.NewSharedProductionPostgresDBQueries(false)
			Expect(err).To(BeNil())
			defer dbQueries.CloseDatabase()

			mapping := &db.APICRToDatabaseMapping{
				APIResourceType: db.APICRToDatabaseMapping_ResourceType_GitOpsDeploymentManagedEnvironment,
				APIResourceUID:  string(managedEnv.UID),
				DBRelationType:  db.APICRToDatabaseMapping_DBRelationType_ManagedEnvironment,
			}
			err = dbQueries.GetDatabaseMappingForAPICR(context.Background(), mapping)
			Expect(err).To(BeNil())

			argoCDClusterSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      argocdutil.GenerateArgoCDClusterSecretName(db.ManagedEnvironment{Managedenvironment_id: mapping.DBRelationKey}),
					Namespace: dbutil.DefaultGitOpsEngineSingleInstanceNamespace,
				},
			}

			Expect(argoCDClusterSecret).To(k8s.ExistByName(k8sClient))

			Expect(string(argoCDClusterSecret.Data["server"])).To(ContainSubstring(clusteragenteventloop.ManagedEnvironmentQueryParameter))

			By("ensuring the resources of the GitOps repo are successfully deployed")

			componentADepl := &apps.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "component-a", Namespace: fixture.GitOpsServiceE2ENamespace},
			}
			componentBDepl := &apps.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "component-b", Namespace: fixture.GitOpsServiceE2ENamespace},
			}
			Eventually(componentADepl, "60s", "1s").Should(k8s.ExistByName(k8sClient))
			Eventually(componentBDepl, "60s", "1s").Should(k8s.ExistByName(k8sClient))

			By("deleting the secret and managed environment")
			err = k8s.Delete(&secret, k8sClient)
			Expect(err).To(BeNil())

			err = k8s.Delete(&managedEnv, k8sClient)
			Expect(err).To(BeNil())

			Eventually(argoCDClusterSecret, "60s", "1s").ShouldNot(k8s.ExistByName(k8sClient),
				"once the ManagedEnvironment is deleted, the Argo CD cluster secret should be deleted as well.")

			app := appv1alpha1.Application{
				ObjectMeta: metav1.ObjectMeta{
					Name:      argocdutil.GenerateArgoCDApplicationName(string(gitOpsDeploymentResource.UID)),
					Namespace: dbutil.GetGitOpsEngineSingleInstanceNamespace(),
				},
			}
			Eventually(app, "60s", "1s").Should(appFixture.HasDestinationField(appv1alpha1.ApplicationDestination{
				Namespace: gitOpsDeploymentResource.Spec.Destination.Namespace,
				Name:      "",
			}), "the Argo CD Application resource's spec.destination field should have an empty environment field")

			By("deleting the GitOpsDeployment")

			err = k8s.Delete(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

		})

		It("should be healthy and have synced status, and resources should be deployed, when deployed with a ManagedEnv using an existing SA", func() {

			serviceAccountName := "gitops-managed-environment-test-service-account"

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			By("creating a ServiceAccount which we will deploy with, using the GitOpsDeploymentManagedEnvironment")
			serviceAccount := corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceAccountName,
					Namespace: fixture.GitOpsServiceE2ENamespace,
				},
			}
			err = k8sClient.Create(context.Background(), &serviceAccount)
			Expect(err).To(Succeed())

			// Now create the cluster role and cluster role binding
			err = createOrUpdateClusterRoleAndRoleBinding(ctx, "123", k8sClient, serviceAccountName, serviceAccount.Namespace, ArgoCDManagerNamespacePolicyRules)
			Expect(err).To(BeNil())

			// Create Service Account and wait for bearer token
			tokenSecret, err := k8s.CreateServiceAccountBearerToken(ctx, k8sClient, serviceAccount.Name, serviceAccount.Namespace)
			Expect(err).To(BeNil())
			Expect(tokenSecret).NotTo(BeNil())

			By("creating the GitOpsDeploymentManagedEnvironment and Secret")

			_, apiServerURL, err := extractKubeConfigValues()
			Expect(err).To(BeNil())

			kubeConfigContents := generateKubeConfig(apiServerURL, fixture.GitOpsServiceE2ENamespace, tokenSecret)

			managedEnv, secret := buildManagedEnvironment(apiServerURL, kubeConfigContents, false)

			err = k8s.Create(&secret, k8sClient)
			Expect(err).To(BeNil())

			err = k8s.Create(&managedEnv, k8sClient)
			Expect(err).To(BeNil())

			By("by creating a GitOpsDeployment pointing to the ManagedEnvironment")

			gitOpsDeploymentResource := buildGitOpsDeploymentResource("my-gitops-depl",
				"https://github.com/redhat-appstudio/managed-gitops",
				"resources/test-data/sample-gitops-repository/environments/overlays/dev",
				managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated)

			gitOpsDeploymentResource.Spec.Destination.Environment = managedEnv.Name
			gitOpsDeploymentResource.Spec.Destination.Namespace = fixture.GitOpsServiceE2ENamespace
			err = k8s.Create(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(BeNil())

			By("ensuring GitOpsDeployment should have expected health and status and reconciledState")

			expectedReconciledStateField := managedgitopsv1alpha1.ReconciledState{
				Source: managedgitopsv1alpha1.GitOpsDeploymentSource{
					RepoURL: gitOpsDeploymentResource.Spec.Source.RepoURL,
					Path:    gitOpsDeploymentResource.Spec.Source.Path,
				},
				Destination: managedgitopsv1alpha1.GitOpsDeploymentDestination{
					Name:      gitOpsDeploymentResource.Spec.Destination.Environment,
					Namespace: gitOpsDeploymentResource.Spec.Destination.Namespace,
				},
			}

			Eventually(gitOpsDeploymentResource, "2m", "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy),
					gitopsDeplFixture.HaveReconciledState(expectedReconciledStateField)))

			secretList := corev1.SecretList{}

			err = k8sClient.List(context.Background(), &secretList, &client.ListOptions{Namespace: dbutil.DefaultGitOpsEngineSingleInstanceNamespace})
			Expect(err).To(BeNil())

			dbQueries, err := db.NewSharedProductionPostgresDBQueries(false)
			Expect(err).To(BeNil())
			defer dbQueries.CloseDatabase()

			mapping := &db.APICRToDatabaseMapping{
				APIResourceType: db.APICRToDatabaseMapping_ResourceType_GitOpsDeploymentManagedEnvironment,
				APIResourceUID:  string(managedEnv.UID),
				DBRelationType:  db.APICRToDatabaseMapping_DBRelationType_ManagedEnvironment,
			}
			err = dbQueries.GetDatabaseMappingForAPICR(context.Background(), mapping)
			Expect(err).To(BeNil())

			argoCDClusterSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      argocdutil.GenerateArgoCDClusterSecretName(db.ManagedEnvironment{Managedenvironment_id: mapping.DBRelationKey}),
					Namespace: dbutil.DefaultGitOpsEngineSingleInstanceNamespace,
				},
			}

			Expect(argoCDClusterSecret).To(k8s.ExistByName(k8sClient))

			By("ensuring the resources of the GitOps repo are successfully deployed")

			componentADepl := &apps.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "component-a", Namespace: fixture.GitOpsServiceE2ENamespace},
			}
			componentBDepl := &apps.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "component-b", Namespace: fixture.GitOpsServiceE2ENamespace},
			}
			Eventually(componentADepl, "60s", "1s").Should(k8s.ExistByName(k8sClient))
			Eventually(componentBDepl, "60s", "1s").Should(k8s.ExistByName(k8sClient))

			By("deleting the secret and managed environment")
			err = k8s.Delete(&secret, k8sClient)
			Expect(err).To(BeNil())

			err = k8s.Delete(&managedEnv, k8sClient)
			Expect(err).To(BeNil())

			Eventually(argoCDClusterSecret, "60s", "1s").ShouldNot(k8s.ExistByName(k8sClient),
				"once the ManagedEnvironment is deleted, the Argo CD cluster secret should be deleted as well.")

			app := appv1alpha1.Application{
				ObjectMeta: metav1.ObjectMeta{
					Name:      argocdutil.GenerateArgoCDApplicationName(string(gitOpsDeploymentResource.UID)),
					Namespace: dbutil.GetGitOpsEngineSingleInstanceNamespace(),
				},
			}
			Eventually(app, "60s", "1s").Should(appFixture.HasDestinationField(appv1alpha1.ApplicationDestination{
				Namespace: gitOpsDeploymentResource.Spec.Destination.Namespace,
				Name:      "",
			}), "the Argo CD Application resource's spec.destination field should have an empty environment field")

			err = k8s.Delete(&serviceAccount, k8sClient)
			Expect(err).To(Succeed())

			By("deleting the GitOpsDeployment")

			err = k8s.Delete(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())
		})

		// Same as previous test but the service account token is not passed through to the managed env
		It("should be unhealthy with no sync status because the managed env does not have a proper token", func() {

			serviceAccountName := "gitops-managed-environment-test-service-account"

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			serviceAccount := corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceAccountName,
					Namespace: fixture.GitOpsServiceE2ENamespace,
				},
			}
			err = k8sClient.Create(context.Background(), &serviceAccount)
			Expect(err).To(Succeed())

			// Now create the cluster role and cluster role binding
			err = createOrUpdateClusterRoleAndRoleBinding(ctx, "123", k8sClient, serviceAccountName, serviceAccount.Namespace, ArgoCDManagerNamespacePolicyRules)
			Expect(err).To(BeNil())

			// Create Service Account and wait for bearer token
			tokenSecret, err := k8s.CreateServiceAccountBearerToken(ctx, k8sClient, serviceAccount.Name, serviceAccount.Namespace)
			Expect(err).To(BeNil())
			Expect(tokenSecret).NotTo(BeNil())

			By("creating the GitOpsDeploymentManagedEnvironment")

			_, apiServerURL, err := extractKubeConfigValues()
			Expect(err).To(BeNil())

			// Set the tokenSecret to be "" to intentionally fail
			kubeConfigContents := generateKubeConfig(apiServerURL, fixture.GitOpsServiceE2ENamespace, "")

			managedEnv, secret := buildManagedEnvironment(apiServerURL, kubeConfigContents, false)

			err = k8s.Create(&secret, k8sClient)
			Expect(err).To(BeNil())

			err = k8s.Create(&managedEnv, k8sClient)
			Expect(err).To(BeNil())

			gitOpsDeploymentResource := buildGitOpsDeploymentResource("my-gitops-depl",
				"https://github.com/redhat-appstudio/managed-gitops",
				"resources/test-data/sample-gitops-repository/environments/overlays/dev",
				managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated)

			gitOpsDeploymentResource.Spec.Destination.Environment = managedEnv.Name
			gitOpsDeploymentResource.Spec.Destination.Namespace = fixture.GitOpsServiceE2ENamespace
			err = k8s.Create(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(BeNil())

			By("ensuring GitOpsDeployment has the expected error condition")

			expectedConditions := []managedgitopsv1alpha1.GitOpsDeploymentCondition{
				{
					Type:    managedgitopsv1alpha1.GitOpsDeploymentConditionErrorOccurred,
					Message: "Unable to reconcile the ManagedEnvironment. Verify that the ManagedEnvironment and Secret are correctly defined, and have valid credentials",
					Status:  managedgitopsv1alpha1.GitOpsConditionStatusTrue,
					Reason:  managedgitopsv1alpha1.GitopsDeploymentReasonErrorOccurred,
				},
			}

			Eventually(gitOpsDeploymentResource, "2m", "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveConditions(expectedConditions),
				),
			)

			By("deleting the secret and managed environment")
			err = k8s.Delete(&secret, k8sClient)
			Expect(err).To(BeNil())

			err = k8s.Delete(&managedEnv, k8sClient)
			Expect(err).To(BeNil())

			err = k8s.Delete(&serviceAccount, k8sClient)
			Expect(err).To(Succeed())

			By("deleting the GitOpsDeployment")

			err = k8s.Delete(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())
		})

		It("should be unhealthy with no sync status because the service account doesn't have sufficient permission", func() {

			serviceAccountName := "gitops-managed-environment-test-service-account"

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			serviceAccount := corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceAccountName,
					Namespace: fixture.GitOpsServiceE2ENamespace,
				},
			}
			err = k8sClient.Create(context.Background(), &serviceAccount)
			Expect(err).To(Succeed())

			insufficientPermissions := []rbacv1.PolicyRule{
				{
					APIGroups: []string{"*"},
					Resources: []string{"Pods"},
					Verbs:     []string{"get", "list"},
				},
			}
			// Now create the cluster role and cluster role binding
			err = createOrUpdateClusterRoleAndRoleBinding(ctx, "123", k8sClient, serviceAccountName, serviceAccount.Namespace, insufficientPermissions)
			Expect(err).To(BeNil())

			// Create Service Account and wait for bearer token
			tokenSecret, err := k8s.CreateServiceAccountBearerToken(ctx, k8sClient, serviceAccount.Name, serviceAccount.Namespace)
			Expect(err).To(BeNil())
			Expect(tokenSecret).NotTo(BeNil())

			By("creating the GitOpsDeploymentManagedEnvironment")

			_, apiServerURL, err := extractKubeConfigValues()
			Expect(err).To(BeNil())

			kubeConfigContents := generateKubeConfig(apiServerURL, fixture.GitOpsServiceE2ENamespace, tokenSecret)

			managedEnv, secret := buildManagedEnvironment(apiServerURL, kubeConfigContents, false)

			err = k8s.Create(&secret, k8sClient)
			Expect(err).To(BeNil())

			err = k8s.Create(&managedEnv, k8sClient)
			Expect(err).To(BeNil())

			gitOpsDeploymentResource := buildGitOpsDeploymentResource("my-gitops-depl",
				"https://github.com/redhat-appstudio/managed-gitops",
				"resources/test-data/sample-gitops-repository/environments/overlays/dev",
				managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated)

			gitOpsDeploymentResource.Spec.Destination.Environment = managedEnv.Name
			gitOpsDeploymentResource.Spec.Destination.Namespace = fixture.GitOpsServiceE2ENamespace
			err = k8s.Create(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(BeNil())

			By("ensuring GitOpsDeployment has the expected error condition")

			expectedConditions := []managedgitopsv1alpha1.GitOpsDeploymentCondition{
				{
					Type:    managedgitopsv1alpha1.GitOpsDeploymentConditionErrorOccurred,
					Message: "Unable to reconcile the ManagedEnvironment. Verify that the ManagedEnvironment and Secret are correctly defined, and have valid credentials",
					Status:  managedgitopsv1alpha1.GitOpsConditionStatusTrue,
					Reason:  managedgitopsv1alpha1.GitopsDeploymentReasonErrorOccurred,
				},
			}

			Eventually(gitOpsDeploymentResource, "2m", "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveConditions(expectedConditions),
				),
			)

			By("deleting the secret and managed environment")
			err = k8s.Delete(&secret, k8sClient)
			Expect(err).To(BeNil())

			err = k8s.Delete(&managedEnv, k8sClient)
			Expect(err).To(BeNil())

			err = k8s.Delete(&serviceAccount, k8sClient)
			Expect(err).To(Succeed())

			By("deleting the GitOpsDeployment")

			err = k8s.Delete(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())
		})
	})
})

var _ = Describe("Environment E2E tests", func() {

	Context("Create a new Environment and checks whether ManagedEnvironment has been created", func() {

		It("should ensure that AllowInsecureSkipTLSVerify field of Environment API is equal to AllowInsecureSkipTLSVerify field of GitOpsDeploymentManagedEnvironment", func() {

			Expect(fixture.EnsureCleanSlate()).To(Succeed())

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			kubeConfigContents, apiServerURL, err := fixture.ExtractKubeConfigValues()
			Expect(err).To(BeNil())

			By("creating managed environment Secret")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-managed-env-secret",
					Namespace: fixture.GitOpsServiceE2ENamespace,
				},
				Type:       "managed-gitops.redhat.com/managed-environment",
				StringData: map[string]string{"kubeconfig": kubeConfigContents},
			}

			err = k8s.Create(secret, k8sClient)
			Expect(err).To(BeNil())

			By("creating the new 'staging' Environment")
			environment := appstudioshared.Environment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "staging",
					Namespace: fixture.GitOpsServiceE2ENamespace,
				},
				Spec: appstudioshared.EnvironmentSpec{
					DisplayName:        "my-environment",
					DeploymentStrategy: appstudioshared.DeploymentStrategy_AppStudioAutomated,
					ParentEnvironment:  "",
					Tags:               []string{},
					Configuration: appstudioshared.EnvironmentConfiguration{
						Env: []appstudioshared.EnvVarPair{},
					},
					UnstableConfigurationFields: &appstudioshared.UnstableEnvironmentConfiguration{
						KubernetesClusterCredentials: appstudioshared.KubernetesClusterCredentials{
							TargetNamespace:            fixture.GitOpsServiceE2ENamespace,
							APIURL:                     apiServerURL,
							ClusterCredentialsSecret:   secret.Name,
							AllowInsecureSkipTLSVerify: true,
						},
					},
				},
			}

			err = k8s.Create(&environment, k8sClient)
			Expect(err).To(Succeed())

			By("checks if managedEnvironment CR has been created and AllowInsecureSkipTLSVerify field is equal to AllowInsecureSkipTLSVerify field of Environment API")
			managedEnvCR := managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "managed-environment-" + environment.Name,
					Namespace: fixture.GitOpsServiceE2ENamespace,
				},
			}

			Eventually(managedEnvCR, "2m", "1s").Should(
				SatisfyAll(
					managedenvironment.HaveAllowInsecureSkipTLSVerify(environment.Spec.UnstableConfigurationFields.AllowInsecureSkipTLSVerify),
				),
			)

			err = k8s.Get(&environment, k8sClient)
			Expect(err).To(BeNil())

			By("update AllowInsecureSkipTLSVerify field of Environment to false and verify whether it updates the AllowInsecureSkipTLSVerify field of GitOpsDeploymentManagedEnvironment")
			environment.Spec.UnstableConfigurationFields = &appstudioshared.UnstableEnvironmentConfiguration{
				KubernetesClusterCredentials: appstudioshared.KubernetesClusterCredentials{
					TargetNamespace:            fixture.GitOpsServiceE2ENamespace,
					APIURL:                     apiServerURL,
					ClusterCredentialsSecret:   secret.Name,
					AllowInsecureSkipTLSVerify: false,
				},
			}

			err = k8s.Update(&environment, k8sClient)
			Expect(err).To(BeNil())

			Eventually(managedEnvCR, "2m", "1s").Should(
				SatisfyAll(
					managedenvironment.HaveAllowInsecureSkipTLSVerify(environment.Spec.UnstableConfigurationFields.AllowInsecureSkipTLSVerify),
				),
			)

		})
	})
})

// extractKubeConfigValues returns contents of k8s config from $KUBE_CONFIG, plus server api url (and error)
func extractKubeConfigValues() (string, string, error) {

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()

	config, err := loadingRules.Load()
	if err != nil {
		return "", "", err
	}

	context, ok := config.Contexts[config.CurrentContext]
	if !ok || context == nil {
		return "", "", fmt.Errorf("no context")
	}

	cluster, ok := config.Clusters[context.Cluster]
	if !ok || cluster == nil {
		return "", "", fmt.Errorf("no cluster")
	}

	var kubeConfigDefault string

	paths := loadingRules.Precedence
	{

		for _, path := range paths {

			GinkgoWriter.Println("Attempting to read kube config from", path)

			// homeDir, err := os.UserHomeDir()
			// if err != nil {
			// 	return "", "", err
			// }

			_, err = os.Stat(path)
			if err != nil {
				GinkgoWriter.Println("Unable to resolve path", path, err)
			} else {
				// Success
				kubeConfigDefault = path
				break
			}

		}

		if kubeConfigDefault == "" {
			return "", "", fmt.Errorf("unable to retrieve kube config path")
		}
	}

	kubeConfigContents, err := os.ReadFile(kubeConfigDefault)
	if err != nil {
		return "", "", err
	}

	return string(kubeConfigContents), cluster.Server, nil
}

func generateKubeConfig(serverURL string, currentNamespace string, token string) string {

	return `
apiVersion: v1
kind: Config
clusters:
  - cluster:
      insecure-skip-tls-verify: true
      server: ` + serverURL + `
    name: cluster-name
contexts:
  - context:
      cluster: cluster-name
      namespace: ` + currentNamespace + `
      user: user-name
    name: context-name
current-context: context-name
preferences: {}
users:
  - name: user-name
    user:
      token: ` + token + `
`

}

func buildManagedEnvironment(apiServerURL string, kubeConfigContents string, createNewServiceAccount bool) (managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironment, corev1.Secret) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-managed-env-secret",
			Namespace: fixture.GitOpsServiceE2ENamespace,
		},
		Type:       "managed-gitops.redhat.com/managed-environment",
		StringData: map[string]string{"kubeconfig": kubeConfigContents},
	}

	managedEnv := &managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-managed-env",
			Namespace: fixture.GitOpsServiceE2ENamespace,
		},
		Spec: managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironmentSpec{
			APIURL:                     apiServerURL,
			ClusterCredentialsSecret:   secret.Name,
			AllowInsecureSkipTLSVerify: true,
			CreateNewServiceAccount:    createNewServiceAccount,
		},
	}

	return *managedEnv, *secret
}

const (
	ArgoCDManagerClusterRoleNamePrefix        = "argocd-manager-cluster-role-"
	ArgoCDManagerClusterRoleBindingNamePrefix = "argocd-manager-cluster-role-binding-"
)

var (
	ArgoCDManagerNamespacePolicyRules = []rbacv1.PolicyRule{
		{
			APIGroups: []string{"*"},
			Resources: []string{"*"},
			Verbs:     []string{"*"},
		},
	}
)

func createOrUpdateClusterRoleAndRoleBinding(ctx context.Context, uuid string, k8sClient client.Client,
	serviceAccountName string, serviceAccountNamespace string, policyRules []rbacv1.PolicyRule) error {

	clusterRole := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: ArgoCDManagerClusterRoleNamePrefix + uuid,
		},
	}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(clusterRole), clusterRole); err != nil {

		clusterRole.Rules = policyRules
		if err := k8sClient.Create(ctx, clusterRole); err != nil {
			return fmt.Errorf("unable to create clusterrole: %w", err)
		}
	}

	clusterRoleBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: ArgoCDManagerClusterRoleBindingNamePrefix + uuid,
		},
	}

	clusterRoleBinding.RoleRef = rbacv1.RoleRef{
		APIGroup: "rbac.authorization.k8s.io",
		Kind:     "ClusterRole",
		Name:     clusterRole.Name,
	}

	clusterRoleBinding.Subjects = []rbacv1.Subject{{
		Kind:      rbacv1.ServiceAccountKind,
		Name:      serviceAccountName,
		Namespace: serviceAccountNamespace,
	}}

	if err := k8sClient.Create(ctx, clusterRoleBinding); err != nil {
		return fmt.Errorf("unable to create clusterrole: %w", err)
	}

	return nil
}
