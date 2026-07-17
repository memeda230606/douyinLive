export namespace app {

	export class CapabilityDTO {
	    id: string;
	    label: string;
	    available: boolean;

	    static createFrom(source: any = {}) {
	        return new CapabilityDTO(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.label = source["label"];
	        this.available = source["available"];
	    }
	}
	export class DataStatusDTO {
	    ready: boolean;
	    schemaVersion: number;
	    mode: string;
	    loggingReady: boolean;

	    static createFrom(source: any = {}) {
	        return new DataStatusDTO(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ready = source["ready"];
	        this.schemaVersion = source["schemaVersion"];
	        this.mode = source["mode"];
	        this.loggingReady = source["loggingReady"];
	    }
	}
	export class BootstrapDTO {
	    apiVersion: string;
	    name: string;
	    version: string;
	    state: string;
	    data: DataStatusDTO;
	    capabilities: CapabilityDTO[];

	    static createFrom(source: any = {}) {
	        return new BootstrapDTO(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.apiVersion = source["apiVersion"];
	        this.name = source["name"];
	        this.version = source["version"];
	        this.state = source["state"];
	        this.data = this.convertValues(source["data"], DataStatusDTO);
	        this.capabilities = this.convertValues(source["capabilities"], CapabilityDTO);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}


}

export namespace room {

	export class CookieStatus {
	    configured: boolean;
	    updatedAt?: number;

	    static createFrom(source: any = {}) {
	        return new CookieStatus(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.configured = source["configured"];
	        this.updatedAt = source["updatedAt"];
	    }
	}
	export class RecordingProfile {
	    quality: string;
	    segmentMinutes: number;
	    saveDirectory?: string;

	    static createFrom(source: any = {}) {
	        return new RecordingProfile(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.quality = source["quality"];
	        this.segmentMinutes = source["segmentMinutes"];
	        this.saveDirectory = source["saveDirectory"];
	    }
	}
	export class CreateRoomInput {
	    liveId: string;
	    alias: string;
	    monitorEnabled: boolean;
	    recordEnabled: boolean;
	    recordingProfile: RecordingProfile;

	    static createFrom(source: any = {}) {
	        return new CreateRoomInput(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.liveId = source["liveId"];
	        this.alias = source["alias"];
	        this.monitorEnabled = source["monitorEnabled"];
	        this.recordEnabled = source["recordEnabled"];
	        this.recordingProfile = this.convertValues(source["recordingProfile"], RecordingProfile);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

	export class RoomConfig {
	    id: string;
	    liveId: string;
	    roomId?: string;
	    alias: string;
	    anchorName?: string;
	    monitorEnabled: boolean;
	    recordEnabled: boolean;
	    recordingProfile: RecordingProfile;
	    cookie: CookieStatus;
	    createdAt: number;
	    updatedAt: number;

	    static createFrom(source: any = {}) {
	        return new RoomConfig(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.liveId = source["liveId"];
	        this.roomId = source["roomId"];
	        this.alias = source["alias"];
	        this.anchorName = source["anchorName"];
	        this.monitorEnabled = source["monitorEnabled"];
	        this.recordEnabled = source["recordEnabled"];
	        this.recordingProfile = this.convertValues(source["recordingProfile"], RecordingProfile);
	        this.cookie = this.convertValues(source["cookie"], CookieStatus);
	        this.createdAt = source["createdAt"];
	        this.updatedAt = source["updatedAt"];
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class RoomRuntimeStatus {
	    roomId: string;
	    liveId: string;
	    alias: string;
	    state: string;
	    operationId?: string;
	    liveName?: string;
	    title?: string;
	    lastCheckedAt?: number;
	    changedAt: number;
	    retryAt?: number;
	    errorCode?: string;
	    message: string;

	    static createFrom(source: any = {}) {
	        return new RoomRuntimeStatus(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.roomId = source["roomId"];
	        this.liveId = source["liveId"];
	        this.alias = source["alias"];
	        this.state = source["state"];
	        this.operationId = source["operationId"];
	        this.liveName = source["liveName"];
	        this.title = source["title"];
	        this.lastCheckedAt = source["lastCheckedAt"];
	        this.changedAt = source["changedAt"];
	        this.retryAt = source["retryAt"];
	        this.errorCode = source["errorCode"];
	        this.message = source["message"];
	    }
	}
	export class SetRoomCookieInput {
	    roomId: string;
	    cookie: string;

	    static createFrom(source: any = {}) {
	        return new SetRoomCookieInput(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.roomId = source["roomId"];
	        this.cookie = source["cookie"];
	    }
	}
	export class UpdateRoomInput {
	    liveId: string;
	    alias: string;
	    monitorEnabled: boolean;
	    recordEnabled: boolean;
	    recordingProfile: RecordingProfile;

	    static createFrom(source: any = {}) {
	        return new UpdateRoomInput(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.liveId = source["liveId"];
	        this.alias = source["alias"];
	        this.monitorEnabled = source["monitorEnabled"];
	        this.recordEnabled = source["recordEnabled"];
	        this.recordingProfile = this.convertValues(source["recordingProfile"], RecordingProfile);
	    }

		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace settings {

	export class AppSettings {
	    version: number;
	    storageRoot: string;
	    recordingDirectory: string;
	    defaultQuality: string;
	    defaultSegmentMinutes: number;
	    maxConcurrentRecordings: number;
	    minimumFreeSpaceGiB: number;
	    saveDisplayNames: boolean;

	    static createFrom(source: any = {}) {
	        return new AppSettings(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.version = source["version"];
	        this.storageRoot = source["storageRoot"];
	        this.recordingDirectory = source["recordingDirectory"];
	        this.defaultQuality = source["defaultQuality"];
	        this.defaultSegmentMinutes = source["defaultSegmentMinutes"];
	        this.maxConcurrentRecordings = source["maxConcurrentRecordings"];
	        this.minimumFreeSpaceGiB = source["minimumFreeSpaceGiB"];
	        this.saveDisplayNames = source["saveDisplayNames"];
	    }
	}
	export class UpdateSettingsInput {
	    recordingDirectory: string;
	    defaultQuality: string;
	    defaultSegmentMinutes: number;
	    maxConcurrentRecordings: number;
	    minimumFreeSpaceGiB: number;
	    saveDisplayNames: boolean;

	    static createFrom(source: any = {}) {
	        return new UpdateSettingsInput(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.recordingDirectory = source["recordingDirectory"];
	        this.defaultQuality = source["defaultQuality"];
	        this.defaultSegmentMinutes = source["defaultSegmentMinutes"];
	        this.maxConcurrentRecordings = source["maxConcurrentRecordings"];
	        this.minimumFreeSpaceGiB = source["minimumFreeSpaceGiB"];
	        this.saveDisplayNames = source["saveDisplayNames"];
	    }
	}

}
