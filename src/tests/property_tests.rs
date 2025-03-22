// #[cfg(test)]
// mod property_tests {
// use crate::app::{AppManager, ContainerRuntime}; // Import ContainerRuntime trait
// use crate::tests::mocks::mocks::{MockContainerRuntime, MockServiceDiscovery};
// use anyhow::Result;
// use std::sync::Arc;

//     // Helper function to create a test AppManager with mocks
//     async fn create_test_app_manager() -> Result<(AppManager, Arc<MockContainerRuntime>, Arc<MockServiceDiscovery>)> {
//         // Create mock components
//         let runtime = Arc::new(MockContainerRuntime::new());
//         let service_discovery = Arc::new(MockServiceDiscovery::new());
        
//         // Create app manager with mock runtime
//         let app_manager = AppManager::new().await?
//             .with_container_runtime(runtime.clone())
//             .with_service_discovery(service_discovery.clone());
        
//         Ok((app_manager, runtime, service_discovery))
//     }

//     #[tokio::test]
//     async fn test_deploy_app() -> Result<()> {
//         let (app, runtime, _) = create_test_app_manager().await?;

//         // Deploy an app
//         let result = app.deploy_app("nginx:latest", "test_app", None, None, None).await;
//         assert!(result.is_ok());

//         // Verify deployment
//         let containers = runtime.list_containers().await?;
//         assert_eq!(containers.len(), 1);
//         assert_eq!(containers[0].name, "test_app");

//         Ok(())
//     }

//     #[tokio::test]
//     async fn test_scale_app() -> Result<()> {
//         let (app, runtime, _) = create_test_app_manager().await?;

//         // Deploy an app
//         let result = app.deploy_app("nginx:latest", "test_app", None, None, None).await;
//         assert!(result.is_ok());

//         // Scale the app
//         let scale_result = app.scale_app("test_app", 3).await;
//         assert!(scale_result.is_ok());

//         // Verify scaling
//         let service = app.get_service("test_app").await;
//         assert!(service.is_some());

//         let service = service.unwrap();
//         assert_eq!(service.current_replicas, 3);
//         assert_eq!(service.container_ids.len(), 3);

//         // Verify containers were created
//         let containers = runtime.list_containers().await?;
//         assert_eq!(containers.len(), 3);

//         Ok(())
//     }

//     #[tokio::test]
//     async fn test_deploy_with_env_vars() -> Result<()> {
//         let (app, runtime, _) = create_test_app_manager().await?;

//         // Deploy an app with environment variables
//         let env_vars = vec![("KEY", "VALUE")];
//         let result = app.deploy_app("nginx:latest", "test_app", None, Some(env_vars), None).await;
//         assert!(result.is_ok());

//         // Verify deployment
//         let containers = runtime.list_containers().await?;
//         assert_eq!(containers.len(), 1);

//         // Verify environment variable
//         let container = &containers[0];
//         assert_eq!(container.env_vars.len(), 1);
//         assert_eq!(container.env_vars[0].key, "KEY");
//         assert_eq!(container.env_vars[0].value, "VALUE");

//         Ok(())
//     }

//     #[tokio::test]
//     async fn test_deploy_with_ports() -> Result<()> {
//         let (app, runtime, _) = create_test_app_manager().await?;

//         // Deploy an app with ports
//         let ports = vec![(8080, 80)];
//         let result = app.deploy_app("nginx:latest", "test_app", None, None, Some(ports)).await;
//         assert!(result.is_ok());

//         // Verify deployment
//         let containers = runtime.list_containers().await?;
//         assert_eq!(containers.len(), 1);
//         let container = &containers[0];

//         // Verify port mapping
//         assert_eq!(container.ports.len(), 1);
//         assert_eq!(container.ports[0].container_port, 8080);
//         assert_eq!(container.ports[0].host_port, 80);

//         Ok(())
//     }
// }
