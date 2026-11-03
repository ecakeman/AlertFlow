from contextlib import asynccontextmanager

from fastapi import FastAPI
from fastapi.responses import JSONResponse


def create_app(runtime) -> FastAPI:
    @asynccontextmanager
    async def lifespan(app: FastAPI):
        app.state.http_ready = True
        yield
        app.state.http_ready = False

    app = FastAPI(lifespan=lifespan)
    app.state.runtime = runtime
    app.state.http_ready = False

    @app.get("/health/live")
    async def live():
        return {"status": "live"}

    @app.get("/health/ready")
    async def ready():
        ok, reason = await runtime.readiness(app.state.http_ready)
        if not ok:
            return JSONResponse({"status": "not_ready", "reason": reason}, status_code=503)
        return {"status": "ready"}

    return app
