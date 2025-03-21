#[cfg(test)]
mod property_tests {
    use crate::app::{AppManager, ServiceConfig, ContainerRuntime};
    use crate::tests::mocks::mocks::{MockContainerRuntime, MockServiceDiscovery};
    use crate::youki_manager::{Container, ContainerStatus, EnvVar, PortMapping, Volume};
    use anyhow::Result;
    use proptest::prelude::*;
    use proptest::test_runner::TestCaseError;
    use std::sync::Arc;
    use tempfile::tempdir;

    // Helper function to create a test AppManager with mocks
    async fn create_test_app_manager() -> Result<(AppManager, Arc<MockContainerRuntime>, Arc<MockServiceDiscovery>)> {
        // Create mock components
        let runtime = Arc::new(MockContainerRuntime::new());
        let service_discovery = Arc::new(MockServiceDiscovery::new());
        
        // Create app manager with mock runtime
        let app_manager = AppManager::new().await?
            .with_container_runtime(runtime.clone())
            .with_service_discovery(service_discovery.clone());
        
        Ok((app_manager, runtime, service_discovery))
    }

    proptest! {
        #[test]
        fn test_container_status_serialization(status in prop::sample::select(
            vec![
                ContainerStatus::Running,
                ContainerStatus::Stopped,
                ContainerStatus::Failed,
                ContainerStatus::Pending,
                ContainerStatus::Restarting
            ]
        )) -> Result<(), TestCaseError> {
            // Ensure that serialization/deserialization preserves the status
            let status_str = status.as_str();
            let deserialized = ContainerStatus::from_str(status_str);
            prop_assert_eq!(status, deserialized);
            Ok(())
        }

        #[test]
        fn test_deploy_app_with_various_names(name in "[a-zA-Z0-9_-]{3,20}") -> Result<(), TestCaseError> {
            let rt = tokio::runtime::Runtime::new().unwrap();
            rt.block_on(async {
                let (app, runtime, _) = create_test_app_manager().await.unwrap();

                // Deploy an app with the generated name
                let result = app.deploy_app("nginx:latest", &name, None, None, None).await;
                prop_assert!(result.is_ok());

                // Verify deployment
                let containers = runtime.list_containers().await.unwrap();
                prop_assert_eq!(containers.len(), 1);
                prop_assert_eq!(containers[0].name, name);
            });
            
            Ok(())
        }
        
        #[test]
        fn test_deploy_app_with_various_domains(
            name in "[a-zA-Z0-9_-]{3,20}",
            domain in "[a-z0-9][a-z0-9-]{1,61}[a-z0-9]\\.[a-z]{2,}"
        ) -> Result<(), TestCaseError> {
            let rt = tokio::runtime::Runtime::new().unwrap();
            rt.block_on(async {
                let (app, runtime, service_discovery) = create_test_app_manager().await.unwrap();

                // Deploy an app with the generated name and domain
                let result = app.deploy_app("nginx:latest", &name, Some(&domain), None, None).await;
                prop_assert!(result.is_ok());

                // Verify deployment
                let containers = runtime.list_containers().await.unwrap();
                prop_assert_eq!(containers.len(), 1);
                prop_assert_eq!(containers[0].name, name);
                
                // Verify service was registered
                let service = app.get_service(&name).await;
                prop_assert!(service.is_some());
                
                let service = service.unwrap();
                prop_assert_eq!(service.name, name);
                prop_assert_eq!(service.domain, domain);
                prop_assert_eq!(service.container_ids.len(), 1);
            });
            
            Ok(())
        }
        
        #[test]
        fn test_scale_app_with_various_replicas(
            name in "[a-zA-Z0-9_-]{3,20}",
            replicas in 1..10u32
        ) -> Result<(), TestCaseError> {
            let rt = tokio::runtime::Runtime::new().unwrap();
            rt.block_on(async {
                let (app, runtime, _) = create_test_app_manager().await.unwrap();

                // Deploy an app with the generated name
                let domain = "test-service.local";
                let result = app.deploy_app("nginx:latest", &name, Some(domain), None, None).await;
                prop_assert!(result.is_ok());
                
                // Scale the app
                let scale_result = app.scale_app(&name, replicas).await;
                prop_assert!(scale_result.is_ok());
                
                // Verify scaling
                let service = app.get_service(&name).await;
                prop_assert!(service.is_some());
                
                let service = service.unwrap();
                prop_assert_eq!(service.current_replicas, replicas);
                prop_assert_eq!(service.container_ids.len() as u32, replicas);
                
                // Verify containers were created
                let containers = runtime.list_containers().await.unwrap();
                prop_assert_eq!(containers.len() as u32, replicas);
            });
            
            Ok(())
        }
        
        #[test]
        fn test_deploy_with_various_env_vars(
            name in "[a-zA-Z0-9_-]{3,20}",
            key in "[A-Z][A-Z0-9_]{2,20}",
            value in "[a-zA-Z0-9_.-]{1,50}"
        ) -> Result<(), TestCaseError> {
            let rt = tokio::runtime::Runtime::new().unwrap();
            rt.block_on(async {
                let (app, runtime, _) = create_test_app_manager().await.unwrap();

                // Deploy an app with the generated environment variable
                let env_vars = vec![(key.as_str(), value.as_str())];
                let result = app.deploy_app("nginx:latest", &name, None, Some(env_vars), None).await;
                prop_assert!(result.is_ok());
                
                // Verify deployment
                let containers = runtime.list_containers().await.unwrap();
                prop_assert_eq!(containers.len(), 1);
                
                // Verify environment variable
                let container = &containers[0];
                prop_assert_eq!(container.env_vars.len(), 1);
                prop_assert_eq!(container.env_vars[0].key, key);
                prop_assert_eq!(container.env_vars[0].value, value);
            });
            
            Ok(())
        }
        
        #[test]
        fn test_deploy_with_various_ports(
            name in "[a-zA-Z0-9_-]{3,20}",
            container_port in 1000..10000u16,
            host_port in 1000..10000u16
        ) -> Result<(), TestCaseError> {
            let rt = tokio::runtime::Runtime::new().unwrap();
            rt.block_on(async {
                let (app, runtime, _) = create_test_app_manager().await.unwrap();

                // Deploy an app with the generated ports
                let ports = vec![(container_port, host_port)];
                let result = app.deploy_app("nginx:latest", &name, None, None, Some(ports)).await;
                prop_assert!(result.is_ok());
                
                // Verify deployment
                let containers = runtime.list_containers().await.unwrap();
                prop_assert_eq!(containers.len(), 1);
                
                // Verify port mapping
                let container = &containers[0];
                prop_assert_eq!(container.ports.len(), 1);
                prop_assert_eq!(container.ports[0].container_port, container_port);
                prop_assert_eq!(container.ports[0].host_port, host_port);
            });
            
            Ok(())
        }
    }
}
