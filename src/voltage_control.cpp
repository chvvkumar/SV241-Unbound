#include "voltage_control.h"
#include "config_manager.h"
#include "hardware_pins.h"

// LEDC (PWM) channel settings
#define LEDC_CHANNEL    0
#define LEDC_FREQUENCY  50000 // 50 kHz - best balance within SC8903 VPWM range (20-100 kHz)
#define LEDC_RESOLUTION 8      // 8-bit resolution (0-255) = ~59mV steps, sufficient for voltage control

// SC8903 uses linear voltage control: VOUT = VOUT_SET × Duty Cycle
// No calibration LUT needed - direct linear relationship per datasheet


// RAM-only target voltage override. -1.0 means "use config".
static float ram_voltage_target = -1.0f;

void setup_voltage_control() {
  // Configure the LEDC peripheral
  ledcSetup(LEDC_CHANNEL, LEDC_FREQUENCY, LEDC_RESOLUTION);

  // Attach the channel to the GPIO pin
  ledcAttachPin(ADJUSTABLE_CONVERTER_PIN, LEDC_CHANNEL);

  xSemaphoreTake(config_mutex, portMAX_DELAY);
  bool startup_state = config.power_startup_states.adj_conv;
  // On startup, we always respect the config preset, so ensure RAM override is cleared.
  ram_voltage_target = -1.0f;
  xSemaphoreGive(config_mutex);

  // Set the initial state based on config
  set_adjustable_converter_state(startup_state);
}

void set_adjustable_converter_state(bool on) {
  if (on) {
    float target_v = 0.0f;
    
    // Check for RAM override first
    if (ram_voltage_target >= 0.0f) {
        target_v = ram_voltage_target;
    } else {
        xSemaphoreTake(config_mutex, portMAX_DELAY);
        target_v = config.adj_conv_preset_v;
        xSemaphoreGive(config_mutex);
    }
    
    // Clamp to max voltage safety limit
    float desired_voltage = min(target_v, (float)ADJUSTABLE_CONVERTER_MAX_VOLTAGE);
    if (desired_voltage < 0.0f) desired_voltage = 0.0f;

    // SC8903 linear voltage control: VOUT = VOUT_SET × Duty Cycle
    // Therefore: Duty Cycle = VOUT / VOUT_SET
    uint32_t max_duty = (1 << LEDC_RESOLUTION) - 1; // 255 for 8-bit
    uint32_t duty_cycle = (desired_voltage / ADJUSTABLE_CONVERTER_MAX_VOLTAGE) * max_duty;
    
    ledcWrite(LEDC_CHANNEL, duty_cycle);
  } else {
    // Set duty cycle to 0 to turn the output off
    ledcWrite(LEDC_CHANNEL, 0);
  }
}

void set_adjustable_voltage_ram(float voltage) {
    if (voltage < 0.0f) voltage = 0.0f;
    if (voltage > ADJUSTABLE_CONVERTER_MAX_VOLTAGE) voltage = ADJUSTABLE_CONVERTER_MAX_VOLTAGE;
    
    ram_voltage_target = voltage;
    
    // If currently on, apply immediately
    // We need to know if it's currently on? 
    // Usually power_control tracks state. We can just re-apply 'true' if we assume it might be on,
    // or we can let the caller handle it. 
    // Better: The caller (power_control) knows the state. But for convenience, let's just trigger update if ON?
    // voltage_control doesn't track ON/OFF state explicitly (stateless function).
    // So the caller must call set_adjustable_converter_state(true) to apply.
}

float get_adjustable_voltage_target() {
    float v = 0.0f;
    if (ram_voltage_target >= 0.0f) {
        v = ram_voltage_target;
    } else {
        xSemaphoreTake(config_mutex, portMAX_DELAY);
        v = config.adj_conv_preset_v;
        xSemaphoreGive(config_mutex);
    }
    return v;
}