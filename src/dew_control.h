#ifndef DEW_CONTROL_H
#define DEW_CONTROL_H

// Public function declarations

/**
 * @brief Initializes the dew heater controllers.
 * Sets up PWM channels and applies startup states.
 */
void setup_dew_heaters();

/**
 * @brief Sets the enabled state of a specific dew heater.
 * 
 * @param heater_index The index of the heater (0 for PWM1, 1 for PWM2).
 * @param enabled True to enable the heater, false to disable it (sets PWM to 0).
 */
void set_dew_heater_state(int heater_index, bool enabled);

/**
 * @brief Gets the current enabled state of a specific dew heater.
 *
 * @param heater_index The index of the heater.
 * @return true if the heater is enabled, false otherwise.
 */
bool get_dew_heater_state(int heater_index);

/**
 * @brief Gets the current power level of a specific dew heater.
 * 
 * @param heater_index The index of the heater.
 * @return The current power level in percent (0-100).
 */
int get_heater_power(int heater_index);

#endif // DEW_CONTROL_H
