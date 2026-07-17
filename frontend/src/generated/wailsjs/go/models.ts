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
