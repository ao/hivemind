use anyhow::Ok;
use axum::{routing::get, Json, Router};

async fn hello() -> &'static str {
    "Hello from Hivemind!"
}

// async fn list_containers(
//     // State is extracted automatically
//     State(mut manager): State<ContainerManager>,
// ) -> Json<Vec<String>> {
//     Json(manager.list_containers().await.unwrap_or_default())
// }

// async fn list_images(
//     State(mut manager): State<ContainerManager>,
// ) -> Json<Vec<String>> {
//     Json(manager.list_images().await.unwrap_or_default())
// }

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    // get args// Parse command-line arguments
    let args: Vec<String> = std::env::args().collect();

    // Check if we're running in daemon mode
    if args.len() > 1 && args[1] == "daemon" {
        // Continue with server setup
        println!("Running in daemon mode");

        let app = Router::new()
            .route("/", get(hello))
            // .route("/containers", get(list_containers))
            // .route("/images", get(list_images))
            .with_state(());

        let listener = tokio::net::TcpListener::bind("127.0.0.1:3000").await?;
        println!("Starting Hivemind web server on http://127.0.0.1:3000");

        axum::serve(listener, app).await?;
    } else {
        // Run in command mode
        if args.len() > 1 {
            match args[1].as_str() {
                "nodes" => {
                    if args.len() > 2 && args[2] == "ls" {
                        println!("Listing nodes...");
                        // Implement node listing logic here

                        return Ok(());
                    }
                }
                "node" => {
                    if args.len() > 2 && args[2] == "remove" {
                        println!("Remove node...");
                        // Implement node listing logic here
                        return Ok(());
                    }
                }
                "containers" => {
                    if args.len() > 2 && args[2] == "ls" {
                        println!("Listing containers...");
                        // Implement node listing logic here
                        return Ok(());
                    }
                }
                "app" => {
                    if args.len() > 2 {
                        if args[2] == "deploy" {
                            println!("Deploy app...");
                            // Implement node listing logic here
                            return Ok(());
                        } else if args[2] == "scale" {
                            println!("Scale app...");
                            // Implement node listing logic here
                            return Ok(());
                        }
                    }
                }
                "health" => {
                    if args.len() > 2 && args[2] == "check" {
                        println!("Checking health status...");
                        // Implement health check logic here
                        return Ok(());
                    }
                }
                _ => {
                    println!("Unknown command. Available commands: nodes ls, check health");
                    return Ok(());
                }
            }
        } else {
            println!("Usage: hivemind daemon | hivemind <command>");
            println!("Commands: nodes ls, check health");
            return Ok(());
        }
    }
    Ok(())
}
